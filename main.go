package main

import (
	"context"
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"net/url"
	"os"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/openfaas-incubator/faas-idler/types"

	providerTypes "github.com/openfaas/faas-provider/types"
	"github.com/openfaas/faas/gateway/metrics"

	sdk "github.com/openfaas/faas-cli/proxy"
)

const (
	scaleLabel     = "com.openfaas.scale.zero"
	emptyNamespace = ""
)

var readOnly bool

var writeDebug bool

var secretMountPath string

//BasicAuth basic authentication for the the gateway
type BasicAuth struct {
	Username string
	Password string
}

//Set set Authorization header on request
func (auth *BasicAuth) Set(req *http.Request) error {
	req.SetBasicAuth(auth.Username, auth.Password)
	return nil
}

//NewHTTPClient returns a new HTTP client
func NewHTTPClient() *http.Client {
	return &http.Client{}
}

func main() {
	config, configErr := types.ReadConfig()
	if configErr != nil {
		log.Panic(configErr.Error())
		os.Exit(1)
	}

	flag.BoolVar(&readOnly, "read-only", false, "use read-only for scaling events [default: false]")
	flag.StringVar(&secretMountPath, "secret-mount-path", "/var/secrets/", "mount path for secrets [default: /var/secrets/]")
	flag.Parse()

	if val, ok := os.LookupEnv("write_debug"); ok && (val == "1" || val == "true") {
		writeDebug = true
	}

	auth := &BasicAuth{}

	if val, ok := os.LookupEnv("secret_mount_path"); ok && len(val) > 0 {
		secretMountPath = val
	}

	if val, err := readFile(path.Join(secretMountPath, "basic-auth-user")); err == nil {
		auth.Username = val
	} else {
		log.Printf("Unable to read username: %s", err)
	}

	if val, err := readFile(path.Join(secretMountPath, "basic-auth-password")); err == nil {
		auth.Password = val
	} else {
		log.Printf("Unable to read password: %s", err)
	}

	timeout := 3 * time.Second

	sdkClient, err := sdk.NewClient(auth, config.GatewayURL, nil, &timeout)
	if err != nil {
		panic(err)
	}

	ctx := context.Background()

	version, err := sdkClient.GetSystemInfo(ctx)
	var (
		release string
		sha     string
	)

	if err != nil {
		log.Printf(err.Error())
	}

	if _, ok := version["orchestration"]; !ok {
		v := version["version"].(map[string]interface{})
		release = v["release"].(string)
		sha = v["sha"].(string)
	}

	log.Printf("Gateway version: %s, SHA: %s\n", release, sha)

	log.Printf(`read_only: %t
gateway_url: %s
inactivity_duration: %s
reconcile_interval: %s
`, readOnly, config.GatewayURL, config.InactivityDuration, config.ReconcileInterval)

	if len(config.GatewayURL) == 0 {
		log.Println("gateway_url (faas-netes/faas-swarm) is required.")
		os.Exit(1)
	}

	for {
		reconcile(ctx, sdkClient, config)
		time.Sleep(config.ReconcileInterval)
		log.Printf("\n")
	}
}

func readFile(path string) (string, error) {
	if _, err := os.Stat(path); err == nil {
		data, readErr := ioutil.ReadFile(path)
		return strings.TrimSpace(string(data)), readErr
	}
	return "", nil
}

func buildMetricsMap(functions []providerTypes.FunctionStatus, config types.Config, namespace string) map[string]float64 {
	query := metrics.NewPrometheusQuery(config.PrometheusHost, config.PrometheusPort, NewHTTPClient())

	duration := fmt.Sprintf("%dm", int(config.InactivityDuration.Minutes()))
	// duration := "5m"
	metricsMap := make(map[string]float64)

	for _, function := range functions {
		//Deriving the function name for multiple namespace support
		sep := ""
		if len(namespace) > 0 {
			sep = "."
		}
		functionName := fmt.Sprintf("%s%s%s", function.Name, sep, namespace)

		querySt := url.QueryEscape(fmt.Sprintf(
			`sum(rate(gateway_function_invocation_started{function_name="%s"}[%s])) by (function_name)`,
			functionName,
			duration))

		log.Printf("Query: %s\n", querySt)

		res, err := query.Fetch(querySt)
		if err != nil {
			log.Println(err)
			continue
		}

		if len(res.Data.Result) > 0 || function.InvocationCount == 0 {

			if _, exists := metricsMap[functionName]; !exists {
				metricsMap[functionName] = 0
			}

			for _, v := range res.Data.Result {

				if writeDebug {
					log.Println(v)
				}

				if v.Metric.FunctionName == functionName {
					metricValue := v.Value[1]
					switch metricValue.(type) {
					case string:

						f, strconvErr := strconv.ParseFloat(metricValue.(string), 64)
						if strconvErr != nil {
							log.Printf("Unable to convert value for metric: %s\n", strconvErr)
							continue
						}

						metricsMap[functionName] = metricsMap[functionName] + f
					}
				}
			}
		}
	}
	return metricsMap
}

func reconcile(ctx context.Context, sdkClient *sdk.Client, config types.Config) {

	//First reconcile with empty namespace for
	// provider like faas-swarm which does not support namespaces
	// and default namespace
	reconcileNamespace(ctx, sdkClient, config, emptyNamespace)

	namespaces, err := sdkClient.ListNamespaces(ctx)
	if err != nil {
		log.Println(err)
		return
	}

	for _, namespace := range namespaces {
		reconcileNamespace(ctx, sdkClient, config, namespace)
	}
}

func reconcileNamespace(ctx context.Context, sdkClient *sdk.Client, config types.Config, namespace string) {
	functions, err := sdkClient.ListFunctions(ctx, namespace)
	if err != nil {
		log.Println(err)
		return
	}

	metricsMap := buildMetricsMap(functions, config, namespace)

	for _, fn := range functions {
		//Deriving the function name for multiple namespace support
		sep := ""
		if len(namespace) > 0 {
			sep = "."
		}
		functionName := fmt.Sprintf("%s%s%s", fn.Name, sep, namespace)

		if fn.Labels != nil {
			labels := *fn.Labels
			labelValue := labels[scaleLabel]

			if labelValue != "1" && labelValue != "true" {
				if writeDebug {
					log.Printf("Skip: %s due to missing label\n", functionName)
				}
				continue
			}
		}

		if v, found := metricsMap[functionName]; found {
			if v == float64(0) {
				log.Printf("%s\tidle\n", functionName)

				val, err := sdkClient.GetFunctionInfo(ctx, fn.Name, namespace)

				if err != nil {
					log.Println(err.Error())
					continue
				}

				replicaCount := uint64(0)
				if readOnly {
					log.Printf("read-only: Scaling %s to %d replicas\n", fn.Name, replicaCount)
					continue
				}

				if err == nil && val.AvailableReplicas > 0 {
					err = sdkClient.ScaleFunction(ctx, fn.Name, namespace, replicaCount)

					if err != nil {
						log.Println(err.Error())
					} else {
						log.Printf("scaled function %s to %d replica(s)\n", fn.Name, replicaCount)
					}
				}

			} else {
				if writeDebug {
					log.Printf("%s\tactive: %f\n", functionName, v)
				}
			}
		}
	}
}

// Version holds the GitHub Release and SHA
type Version struct {
	Version struct {
		Release string `json:"release"`
		SHA     string `json:"sha"`
	}
}
