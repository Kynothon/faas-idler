package main

import (
	"bytes"
	"encoding/json"
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
)

const scaleLabel = "com.openfaas.scale.zero"

var dryRun bool

var writeDebug bool

type Credentials struct {
	Username string
	Password string
}

func main() {
	config, configErr := types.ReadConfig()
	if configErr != nil {
		log.Panic(configErr.Error())
		os.Exit(1)
	}

	flag.BoolVar(&dryRun, "dry-run", false, "use dry-run for scaling events")
	flag.Parse()

	if val, ok := os.LookupEnv("write_debug"); ok && (val == "1" || val == "true") {
		writeDebug = true
	}

	credentials := Credentials{}

	secretMountPath := "/var/secrets/"
	if val, ok := os.LookupEnv("secret_mount_path"); ok && len(val) > 0 {
		secretMountPath = val
	}

	if val, err := readFile(path.Join(secretMountPath, "basic-auth-user")); err == nil {
		credentials.Username = val
	} else {
		log.Printf("Unable to read username: %s", err)
	}

	if val, err := readFile(path.Join(secretMountPath, "basic-auth-password")); err == nil {
		credentials.Password = val
	} else {
		log.Printf("Unable to read password: %s", err)
	}

	client := &http.Client{}
	version, err := getVersion(client, config.GatewayURL, &credentials)

	if err != nil {
		panic(err)
	}

	log.Printf("Gateway version: %s, SHA: %s\n", version.Version.Release, version.Version.SHA)

	fmt.Printf(`dry_run: %t
gateway_url: %s
inactivity_duration: %s
reconcile_interval: %s
`, dryRun, config.GatewayURL, config.InactivityDuration, config.ReconcileInterval)

	if len(config.GatewayURL) == 0 {
		fmt.Println("gateway_url (faas-netes/faas-swarm) is required.")
		os.Exit(1)
	}

	for {
		reconcile(client, config, &credentials)
		time.Sleep(config.ReconcileInterval)
		fmt.Printf("\n")
	}
}

func readFile(path string) (string, error) {
	if _, err := os.Stat(path); err == nil {
		data, readErr := ioutil.ReadFile(path)
		return strings.TrimSpace(string(data)), readErr
	}
	return "", nil
}

func buildMetricsMap(client *http.Client, functions []providerTypes.FunctionStatus, config types.Config, namespace string) map[string]float64 {
	query := metrics.NewPrometheusQuery(config.PrometheusHost, config.PrometheusPort, client)

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

		querySt := url.QueryEscape(`sum(rate(gateway_function_invocation_total{function_name="` + functionName + `", code=~".*"}[` + duration + `])) by (code, function_name)`)

		res, err := query.Fetch(querySt)
		if err != nil {
			log.Println(err)
			continue
		}

		// log.Println(res, function.InvocationCount)
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

func reconcile(client *http.Client, config types.Config, credentials *Credentials) {

	//First reconcile with empty namespace for
	// provider like faas-swarm which does not support namespaces
	// and default namespace
	reconcileNamespace(client, config, credentials, "")

	namespaces, err := getNamespaces(client, config.GatewayURL, credentials)
	if err != nil {
		log.Println(err)
		return
	}

	for _, namespace := range namespaces {
		reconcileNamespace(client, config, credentials, namespace)
	}
}

func reconcileNamespace(client *http.Client, config types.Config, credentials *Credentials, namespace string) {
	functions, err := queryFunctions(client, config.GatewayURL, namespace, credentials)
	if err != nil {
		log.Println(err)
		return
	}

	metricsMap := buildMetricsMap(client, functions, config, namespace)

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
				fmt.Printf("%s\tidle\n", functionName)

				if val, _ := getReplicas(client, config.GatewayURL, fn.Name, namespace, credentials); val != nil && val.AvailableReplicas > 0 {
					sendScaleEvent(client, config.GatewayURL, fn.Name, namespace, uint64(0), credentials)
				}

			} else {
				if writeDebug {
					fmt.Printf("%s\tactive: %f\n", functionName, v)
				}
			}
		}
	}
}

func getNamespaces(client *http.Client, gatewayURL string, credentials *Credentials) ([]string, error) {
	var (
		err        error
		namespaces []string
	)
	req, err := http.NewRequest(http.MethodGet, fmt.Sprintf("%s/system/namespaces", gatewayURL), nil)
	req.SetBasicAuth(credentials.Username, credentials.Password)
	if err != nil {
		return namespaces, err
	}

	res, err := client.Do(req)
	if err != nil {
		return namespaces, err
	}

	if res.Body != nil {
		defer res.Body.Close()
	}

	if res.StatusCode != http.StatusNotFound {
		bytesOut, err := ioutil.ReadAll(res.Body)
		if err != nil {
			return namespaces, err
		}

		if len(bytesOut) == 0 {
			return namespaces, nil
		}

		err = json.Unmarshal(bytesOut, &namespaces)
		if err != nil {
			return namespaces, err
		}
	}
	return namespaces, err
}

func getReplicas(client *http.Client, gatewayURL, name, namespace string, credentials *Credentials) (*providerTypes.FunctionStatus, error) {
	item := &providerTypes.FunctionStatus{}
	var err error

	functionURL := gatewayURL + "system/function/" + name
	if len(namespace) > 0 {
		functionURL, err = addNamespaceParam(functionURL, namespace)
	}

	req, _ := http.NewRequest(http.MethodGet, functionURL, nil)

	req.SetBasicAuth(credentials.Username, credentials.Password)

	res, err := client.Do(req)
	if err != nil {
		return nil, err
	}

	if res.Body != nil {
		defer res.Body.Close()
	}

	bytesOut, _ := ioutil.ReadAll(res.Body)

	err = json.Unmarshal(bytesOut, &item)

	return item, err
}

func queryFunctions(client *http.Client, gatewayURL, namespace string, credentials *Credentials) ([]providerTypes.FunctionStatus, error) {
	list := []providerTypes.FunctionStatus{}
	var err error

	functionsURL := gatewayURL + "system/functions"
	if len(namespace) > 0 {
		functionsURL, err = addNamespaceParam(functionsURL, namespace)
	}

	req, _ := http.NewRequest(http.MethodGet, functionsURL, nil)
	req.SetBasicAuth(credentials.Username, credentials.Password)

	res, err := client.Do(req)
	if err != nil {
		return nil, err
	}

	if res.Body != nil {
		defer res.Body.Close()
	}

	bytesOut, _ := ioutil.ReadAll(res.Body)

	err = json.Unmarshal(bytesOut, &list)

	return list, err
}

func sendScaleEvent(client *http.Client, gatewayURL, name, namespace string, replicas uint64, credentials *Credentials) {
	if dryRun {
		log.Printf("dry-run: Scaling %s to %d replicas\n", name, replicas)
		return
	}

	scaleReq := providerTypes.ScaleServiceRequest{
		ServiceName: name,
		Replicas:    replicas,
	}

	var err error

	bodyBytes, _ := json.Marshal(scaleReq)
	bodyReader := bytes.NewReader(bodyBytes)

	functionURL := gatewayURL + "system/scale-function/" + name
	if len(namespace) > 0 {
		functionURL, err = addNamespaceParam(functionURL, namespace)
	}
	req, _ := http.NewRequest(http.MethodPost, functionURL, bodyReader)
	req.SetBasicAuth(credentials.Username, credentials.Password)

	res, err := client.Do(req)

	if err != nil {
		log.Println(err)
		return
	}
	log.Println("Scale", name, res.StatusCode, replicas)

	if res.Body != nil {
		defer res.Body.Close()
	}
}

// Version holds the GitHub Release and SHA
type Version struct {
	Version struct {
		Release string `json:"release"`
		SHA     string `json:"sha"`
	}
}

func getVersion(client *http.Client, gatewayURL string, credentials *Credentials) (Version, error) {
	version := Version{}
	var err error

	req, _ := http.NewRequest(http.MethodGet, gatewayURL+"system/info", nil)
	req.SetBasicAuth(credentials.Username, credentials.Password)

	res, err := client.Do(req)
	if err != nil {
		return version, err
	}

	if res.Body != nil {
		defer res.Body.Close()
	}

	bytesOut, _ := ioutil.ReadAll(res.Body)

	err = json.Unmarshal(bytesOut, &version)

	return version, err
}

func addNamespaceParam(u, namespace string) (string, error) {
	gatewayURL, err := url.Parse(u)
	if err != nil {
		return u, err
	}

	q := gatewayURL.Query()
	q.Set("namespace", namespace)
	gatewayURL.RawQuery = q.Encode()
	return gatewayURL.String(), nil
}
