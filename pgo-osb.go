package main

/*
Copyright 2018 Crunchy Data Solutions, Inc.
Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

import (
	"context"
	"flag"
	"log"
	"os"
	"os/signal"
	"path"
	"strconv"
	"syscall"

	bridge "github.com/crunchydata/pgo-osb/pkg/osb-bridge"

	crv1 "github.com/crunchydata/postgres-operator/pkg/apis/crunchydata.com/v1"
	"github.com/gofrs/uuid"
	"github.com/pmorie/osb-broker-lib/pkg/metrics"
	"github.com/pmorie/osb-broker-lib/pkg/rest"
	"github.com/pmorie/osb-broker-lib/pkg/server"
	prom "github.com/prometheus/client_golang/prometheus"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	clientset "k8s.io/client-go/kubernetes"
	clientrest "k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

var options struct {
	bridge.Options

	Port                 int
	Insecure             bool
	TLSCert              string
	TLSKey               string
	TLSCertFile          string
	TLSKeyFile           string
	AuthenticateK8SToken bool
	KubeConfig           string
}

func main() {

	//func init() {
	flag.IntVar(&options.Port, "port", 8443, "use '--port' option to specify the port for broker to listen on")
	flag.BoolVar(&options.Insecure, "insecure", false, "use --insecure to use HTTP vs HTTPS.")
	flag.StringVar(&options.TLSCertFile, "tls-cert-file", "", "File containing the default x509 Certificate for HTTPS. (CA cert, if any, concatenated after server cert).")
	flag.StringVar(&options.TLSKeyFile, "tls-private-key-file", "", "File containing the default x509 private key matching --tls-cert-file.")
	flag.StringVar(&options.TLSCert, "tlsCert", "", "base-64 encoded PEM block to use as the certificate for TLS. If '--tlsCert' is used, then '--tlsKey' must also be used.")
	flag.StringVar(&options.TLSKey, "tlsKey", "", "base-64 encoded PEM block to use as the private key matching the TLS certificate.")
	flag.BoolVar(&options.AuthenticateK8SToken, "authenticate-k8s-token", false, "former option to specify if the broker should validate the bearer auth token with kubernetes, disabled 4.6+")
	flag.StringVar(&options.KubeConfig, "kube-config", "", "specify the kube config path to be used")
	bridge.AddFlags(&options.Options)

	flag.Parse()

	log.SetOutput(os.Stdout)

	if options.PGO_OSB_GUID == "" {
		u, err := uuid.NewV4()
		if err != nil {
			log.Printf("PGO_OSB_GUID not set and received error generating one: %s\n", err)
			return
		}
		options.PGO_OSB_GUID = u.String()

		log.Print("generating GUID for this broker since none was supplied in the PGO_OSB_GUID env var: GUID is " + options.PGO_OSB_GUID)
	}

	if err := run(); err != nil && err != context.Canceled && err != context.DeadlineExceeded {
		log.Print(err)
	}
}

func run() error {
	ctx, cancelFunc := context.WithCancel(context.Background())
	defer cancelFunc()
	go cancelOnInterrupt(ctx, cancelFunc)

	return runWithContext(ctx)
}

func runWithContext(ctx context.Context) error {
	if flag.Arg(0) == "version" {
		log.Printf("%s/%s\n", path.Base(os.Args[0]), "0.1.0")
		return nil
	}
	if (options.TLSCert != "" || options.TLSKey != "") &&
		(options.TLSCert == "" || options.TLSKey == "") {
		log.Print("To use TLS with specified cert or key data, both --tlsCert and --tlsKey must be used")
		return nil
	}

	addr := ":" + strconv.Itoa(options.Port)

	RESTClient, err := getRestClient(options.KubeConfig)
	if err != nil {
		return err
	}
	options.Options.KubeAPIClient = RESTClient

	businessLogic, err := bridge.NewBusinessLogic(options.Options)
	if err != nil {
		return err
	}

	// Prom. metrics
	reg := prom.NewRegistry()
	osbMetrics := metrics.New()
	reg.MustRegister(osbMetrics)

	api, err := rest.NewAPISurface(businessLogic, osbMetrics)
	if err != nil {
		return err
	}

	s := server.New(api, reg)
	if options.AuthenticateK8SToken {
		// Avoid breaking invocations using this flag, but this features
		// needs updating to current authenication standards beyond the long
		// out-of-date token review middleware previously used
		log.Print("option AuthenticateK8SToken has no effect in 4.6+")
	}

	log.Print("Starting broker!")

	if options.Insecure {
		err = s.Run(ctx, addr)
	} else {
		if options.TLSCert != "" && options.TLSKey != "" {
			log.Print("Starting secure broker with TLS cert and key data")
			err = s.RunTLS(ctx, addr, options.TLSCert, options.TLSKey)
		} else {
			if options.TLSCertFile == "" || options.TLSKeyFile == "" {
				log.Print("unable to run securely without TLS Certificate and Key. Please review options and if running with TLS, specify --tls-cert-file and --tls-private-key-file or --tlsCert and --tlsKey.")
				return nil
			}
			log.Print("Starting secure broker with file based TLS cert and key")
			err = s.RunTLSWithTLSFiles(ctx, addr, options.TLSCertFile, options.TLSKeyFile)
		}
	}
	return err
}

func getKubernetesConfig(kubeConfigPath string) (*clientrest.Config, error) {
	var clientConfig *clientrest.Config
	var err error
	if kubeConfigPath == "" {
		clientConfig, err = clientrest.InClusterConfig()
		if err != nil {
			return nil, err
		}
	} else {
		config, err := clientcmd.LoadFromFile(kubeConfigPath)
		if err != nil {
			return nil, err
		}

		clientConfig, err = clientcmd.NewDefaultClientConfig(*config, &clientcmd.ConfigOverrides{}).ClientConfig()
		if err != nil {
			return nil, err
		}
	}
	return clientConfig, nil
}

func getKubernetesClient(kubeConfigPath string) (clientset.Interface, error) {
	kubeConfig, err := getKubernetesConfig(kubeConfigPath)
	if err != nil {
		return nil, err
	}
	return clientset.NewForConfig(kubeConfig)
}

func getRestClient(kubeConfigPath string) (*clientrest.RESTClient, error) {

	kubeConfig, err := getKubernetesConfig(kubeConfigPath)
	if err != nil {
		return nil, err
	}

	restClient, _, err := newClient(kubeConfig)
	if err != nil {
		return nil, err
	}

	return restClient, nil
}

// newClient gets a REST connection to Kubernetes. This is imported from an
// older version of the Operator and likely just needs to be redone
func newClient(cfg *clientrest.Config) (*clientrest.RESTClient, *runtime.Scheme, error) {
	scheme := runtime.NewScheme()
	if err := crv1.AddToScheme(scheme); err != nil {
		return nil, nil, err
	}

	config := *cfg
	config.GroupVersion = &crv1.SchemeGroupVersion
	config.APIPath = "/apis"
	config.ContentType = runtime.ContentTypeJSON
	// From pkg.go.dev: "NegotiatedSerializer will be phased out as internal clients are removed from Kubernetes"
	config.NegotiatedSerializer = serializer.NewCodecFactory(scheme).WithoutConversion()

	client, err := clientrest.RESTClientFor(&config)
	if err != nil {
		return nil, nil, err
	}

	return client, scheme, nil
}

func cancelOnInterrupt(ctx context.Context, f context.CancelFunc) {
	term := make(chan os.Signal)
	signal.Notify(term, os.Interrupt, syscall.SIGTERM)

	for {
		select {
		case <-term:
			log.Print("Received SIGTERM, exiting gracefully...")
			f()
			os.Exit(0)
		case <-ctx.Done():
			os.Exit(0)
		}
	}
}
