package main

import (
	"bufio"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/crypto/ssh/terminal"
	"k8s.io/client-go/kubernetes"
	restclient "k8s.io/client-go/rest"
	"k8s.io/helm/pkg/helm"
	"k8s.io/helm/pkg/helm/portforwarder"
	"k8s.io/helm/pkg/kube"
	"k8s.io/helm/pkg/tiller/environment"

	"syscall"

	"github.com/Azure/draft/cmd/draft/installer"
	installerConfig "github.com/Azure/draft/cmd/draft/installer/config"
	"github.com/Azure/draft/pkg/draft/draftpath"
	"github.com/Azure/draft/pkg/draft/pack"
)

const (
	initDesc = `
This command installs the server side component of Draft onto your
Kubernetes Cluster and sets up local configuration in $DRAFT_HOME (default ~/.draft/)

To set up just a local environment, use '--client-only'. That will configure
$DRAFT_HOME, but not attempt to connect to a remote cluster and install the Draft
deployment.

To dump information about the Draft chart, combine the '--dry-run' and '--debug' flags.
`
	chartConfigTpl = `
ingress:
  enabled: %s
  basedomain: %s
registry:
  url: %s
  authtoken: %s
`
)

type initCmd struct {
	clientOnly     bool
	out            io.Writer
	in             io.Reader
	home           draftpath.Home
	autoAccept     bool
	helmClient     *helm.Client
	ingressEnabled bool
}

func newInitCmd(out io.Writer, in io.Reader) *cobra.Command {
	i := &initCmd{
		out: out,
		in:  in,
	}

	cmd := &cobra.Command{
		Use:   "init",
		Short: "initialize Draft on both client and server",
		Long:  initDesc,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) != 0 {
				return errors.New("This command does not accept arguments")
			}
			i.home = draftpath.Home(homePath())
			return i.run()
		},
	}

	f := cmd.Flags()
	f.BoolVarP(&i.clientOnly, "client-only", "c", false, "install local configuration, but skip remote configuration")
	f.BoolVarP(&i.ingressEnabled, "ingress-enabled", "i", false, "configure ingress")
	f.BoolVar(&i.autoAccept, "auto-accept", false, "automatically accept configuration defaults (if detected). It will still prompt for information if this is set to true and no cloud provider was found")

	return cmd
}

// runInit initializes local config and installs Draft to Kubernetes Cluster
func (i *initCmd) run() error {

	if err := setupDraftHome(i.home, i.out); err != nil {
		return err
	}
	fmt.Fprintf(i.out, "$DRAFT_HOME has been configured at %s.\n", draftHome)

	if !i.clientOnly {
		client, clientConfig, err := getKubeClient(kubeContext)
		if err != nil {
			return fmt.Errorf("Could not get a kube client: %s", err)
		}
		restClientConfig, err := clientConfig.ClientConfig()
		if err != nil {
			return fmt.Errorf("Could not retrieve client config from the kube client: %s", err)
		}

		tunnel, err := setupTillerConnection(client, restClientConfig)
		if err != nil {
			return err
		}

		i.helmClient = helm.NewClient(helm.Host(fmt.Sprintf("localhost:%d", tunnel.Local)))

		chartConfig, cloudProvider, err := installerConfig.FromClientConfig(clientConfig)
		if err != nil {
			return fmt.Errorf("Could not generate chart config from kube client config: %s", err)
		}

		if cloudProvider != "" {
			fmt.Fprintf(i.out, "\nDraft detected that you are using %s as your cloud provider. AWESOME!\n", cloudProvider)

			if !i.autoAccept {
				fmt.Fprint(i.out, "Is it okay to use the registry addon in minikube to store your application images? If not, we will prompt you for information on the registry you'd like to push your application images to during development. [Y/n] ")
				reader := bufio.NewReader(i.in)
				text, err := reader.ReadString('\n')
				if err != nil {
					return fmt.Errorf("Could not read input: %s", err)
				}
				text = strings.TrimSpace(text)
				if text == "" || strings.ToLower(text) == "y" {
					i.autoAccept = true
				}
			}
		}

		if !i.autoAccept || cloudProvider == "" {
			// prompt for missing information
			fmt.Fprintf(i.out, "\nIn order to configure Draft, we need a bit more information...\n\n")
			fmt.Fprint(i.out, "1. Enter your Docker registry URL (e.g. docker.io/myuser, quay.io/myuser, myregistry.azurecr.io): ")
			reader := bufio.NewReader(i.in)
			registryURL, err := reader.ReadString('\n')
			if err != nil {
				return fmt.Errorf("Could not read input: %s", err)
			}
			registryURL = strings.TrimSpace(registryURL)
			fmt.Fprint(i.out, "2. Enter your username: ")
			dockerUser, err := reader.ReadString('\n')
			if err != nil {
				return fmt.Errorf("Could not read input: %s", err)
			}
			dockerUser = strings.TrimSpace(dockerUser)
			fmt.Fprint(i.out, "3. Enter your password: ")
			// NOTE(bacongobbler): casting syscall.Stdin here to an int is intentional here as on
			// Windows, syscall.Stdin is a Handler, which is of type uintptr.
			dockerPass, err := terminal.ReadPassword(int(syscall.Stdin))
			fmt.Println("")
			if err != nil {
				return fmt.Errorf("Could not read input: %s", err)
			}

			basedomain, err := setBasedomain(i.out, reader, i.ingressEnabled)
			if err != nil {
				return err
			}

			registryAuth := base64.StdEncoding.EncodeToString(
				[]byte(fmt.Sprintf(
					`{"username":"%s","password":"%s"}`,
					dockerUser,
					dockerPass)))

			chartConfig.Raw = fmt.Sprintf(chartConfigTpl, strconv.FormatBool(i.ingressEnabled), basedomain, registryURL, registryAuth)
		}

		if err := installer.Install(i.helmClient, chartConfig); err != nil {
			if IsReleaseAlreadyExists(err) {
				fmt.Fprintln(i.out, "Warning: Draft is already installed in the cluster.\n"+
					"Use --client-only to suppress this message.")
			} else {
				return fmt.Errorf("error installing Draft: %s", err)
			}
		} else {
			fmt.Fprintln(i.out, "Draft has been installed into your Kubernetes Cluster.")
		}
	} else {
		fmt.Fprintln(i.out, "Skipped installing Draft's server side component in Kubernetes due to 'client-only' flag having been set")
	}

	fmt.Fprintln(i.out, "Happy Sailing!")
	return nil
}

// ensureDirectories checks to see if $DRAFT_HOME exists
//
// If $DRAFT_HOME does not exist, this function will create it.
func ensureDirectories(home draftpath.Home, out io.Writer) error {
	configDirectories := []string{
		home.String(),
		home.Plugins(),
		home.Packs(),
	}
	for _, p := range configDirectories {
		if fi, err := os.Stat(p); err != nil {
			fmt.Fprintf(out, "Creating %s \n", p)
			if err := os.MkdirAll(p, 0755); err != nil {
				return fmt.Errorf("Could not create %s: %s", p, err)
			}
		} else if !fi.IsDir() {
			return fmt.Errorf("%s must be a directory", p)
		}
	}

	return nil
}

// ensurePacks checks to see if the default packs exist.
//
// If the pack does not exist, this function will create it.
func ensurePacks(home draftpath.Home, out io.Writer) error {
	all, err := pack.Builtins()
	if err != nil {
		return err
	}
	for packName, files := range all {
		fmt.Fprintf(out, "Creating pack %s...\n", packName)
		if _, err := pack.Create(packName, home.Packs(), files); err != nil {
			if err == pack.ErrPackExists {
				fmt.Fprintf(out, "Pack %s already exists. Skipping!\n", packName)
			} else {
				return err
			}
		}
	}
	return nil
}

// IsReleaseAlreadyExists returns true if err matches the "release already exists"
// error from Helm; else returns false
func IsReleaseAlreadyExists(err error) bool {
	alreadyExistsRegExp := regexp.MustCompile("a release named \"(([A-Za-z0-9][-A-Za-z0-9_.]*)?[A-Za-z0-9])+\" already exists")
	return alreadyExistsRegExp.MatchString(err.Error())
}

func setupDraftHome(home draftpath.Home, out io.Writer) error {
	if err := ensureDirectories(home, out); err != nil {
		return err
	}

	if err := ensurePacks(home, out); err != nil {
		return err
	}

	return nil
}

func setupTillerConnection(client kubernetes.Interface, restClientConfig *restclient.Config) (*kube.Tunnel, error) {
	tunnel, err := portforwarder.New(environment.DefaultTillerNamespace, client, restClientConfig)
	if err != nil {
		return nil, fmt.Errorf("Could not get a connection to tiller: %s\nPlease ensure you have run `helm init`", err)
	}

	return tunnel, err
}

func setBasedomain(out io.Writer, reader *bufio.Reader, ingress bool) (string, error) {
	if ingress {
		fmt.Fprint(out, "4. Enter your top-level domain for ingress (e.g. draft.example.com): ")
		basedomain, err := reader.ReadString('\n')
		if err != nil {
			return basedomain, fmt.Errorf("Could not read input: %s", err)
		}
		return strings.TrimSpace(basedomain), nil
	} else {
		return "", nil
	}
}
