/*
Copyright 2022 The KServe Authors.

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

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"os"
	"strings"
	"time"

	"k8s.io/client-go/util/jsonpath"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	"math/rand"

	"github.com/kserve/kserve/pkg/apis/serving/v1alpha1"
	flag "github.com/spf13/pflag"
)

var log = logf.Log.WithName("InferenceGraphRouter")

func callService(serviceUrl string, input []byte, res chan<- []byte) error {
	resp, err := http.Post(serviceUrl, "application/json", bytes.NewBuffer(input))
	if err != nil {
		log.Error(err, "An error has occurred for service %s", serviceUrl)
		return err
	}
	defer resp.Body.Close()
	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		log.Error(err, "error while reading the response")
		return err
	}
	res <- body
	return nil
}

func pickupRoute(routes []v1alpha1.InferenceStep) *v1alpha1.InferenceStep {
	r := rand.New(rand.NewSource(time.Now().UnixNano()))
	//generate num [0,100)
	point := r.Intn(99)
	start, end := 0, 0

	for _, route := range routes {
		start += end
		end += int(*route.Weight)
		if point >= start && point < end {
			return &route
		}
	}
	return nil
}

//Input is a struct that can be parsed by jsonpath
type Input struct {
	Items []interface{} `json:"items"`
}

func convertInput(input []byte) (interface{}, error) {
	var inputData interface{}
	if err := json.Unmarshal(input, &inputData); err != nil {
		return nil, err
	}
	var data Input
	data.Items = append(data.Items, inputData)
	return data, nil
}

func convertCondition(origin string) string {
	//remove whitespaces
	str := strings.Replace(origin, " ", "", -1)
	//remove {}
	str = str[1 : len(str)-1]
	return fmt.Sprintf("{@.items[?(%s)]}", str[strings.Index(str, "."):])
}

func pickupRouteByCondition(input []byte, routes []v1alpha1.InferenceStep) *v1alpha1.InferenceStep {
	//convert input to Input
	data, err := convertInput(input)
	if err != nil {
		log.Error(err, "convertInput failed.")
		return nil
	}
	for _, route := range routes {
		j := jsonpath.New("Parser")
		//j.AllowMissingKeys(true)
		cond := convertCondition(route.Condition)
		if err := j.Parse(cond); err != nil {
			log.Error(err, "jsonpath.Parse failed")
			continue
		}
		buf := new(bytes.Buffer)
		if err := j.Execute(buf, data); err != nil {
			log.Error(err, "jsonpath.Execute failed")
		}
		if buf.Len() > 0 { // find the target
			return &route
		}
	}
	return nil
}

func timeTrack(start time.Time, name string) {
	elapsed := time.Since(start)
	log.Info("elapsed time", "node", name, "time", elapsed)
}

func routeStep(nodeName string, currentNode v1alpha1.InferenceRouter, graph v1alpha1.InferenceGraphSpec, input []byte, res chan<- []byte) error {
	log.Info("current step", "nodeName", nodeName)
	defer timeTrack(time.Now(), nodeName)
	response := map[string]interface{}{}
	if currentNode.RouterType == v1alpha1.Splitter {
		result := make(chan []byte)
		route := pickupRoute(currentNode.Routes)
		if route.NodeName != "" {
			go routeStep(route.NodeName, graph.Nodes[route.NodeName], graph, input, result)
		} else {
			go callService(pickupRoute(currentNode.Routes).ServiceUrl, input, result)
		}
		responseBytes := <-result
		var res map[string]interface{}
		json.Unmarshal(responseBytes, &res)
		response = res
	} else if currentNode.RouterType == v1alpha1.Switch {
		route := pickupRouteByCondition(input, currentNode.Routes)
		if route != nil {
			result := make(chan []byte)
			var res map[string]interface{}
			if route.NodeName != "" {
				go routeStep(route.NodeName, graph.Nodes[route.NodeName], graph, input, result)
			} else {
				go callService(route.ServiceUrl, input, result)
			}
			responseBytes := <-result
			json.Unmarshal(responseBytes, &res)
			response = res
		}
	} else if currentNode.RouterType == v1alpha1.Ensemble {
		ensembleRes := map[string]chan []byte{}

		for i := range currentNode.Routes {
			step := currentNode.Routes[i]
			res := make(chan []byte)
			ensembleRes[step.StepName] = res
			if step.NodeName != "" {
				go routeStep(step.NodeName, graph.Nodes[step.NodeName], graph, input, res)
			} else {
				go callService(step.ServiceUrl, input, res)
			}
		}
		// merge responses from parallel steps
		for name, result := range ensembleRes {
			responseBytes := <-result
			var res map[string]interface{}
			json.Unmarshal(responseBytes, &res)
			response[name] = res
		}
	} else if currentNode.RouterType == v1alpha1.Sequence {
		request := input
		var responseBytes []byte
		for i := range currentNode.Routes {
			step := currentNode.Routes[i]
			result := make(chan []byte)
			if step.Data == "$response" && i > 0 {
				request = responseBytes
			}
			// when nodeName is specified make a recursive call for routing to next step
			if step.NodeName != "" {
				go routeStep(step.NodeName, graph.Nodes[step.NodeName], graph, request, result)
			} else {
				go callService(step.ServiceUrl, request, result)
			}
			responseBytes = <-result
		}
		var res map[string]interface{}
		json.Unmarshal(responseBytes, &res)
		response = res
	} else {
		log.Error(fmt.Errorf("invalid route type"), "invalid route type")
	}
	jsonRes, err := json.Marshal(response)
	if err != nil {
		return err
	} else {
		res <- jsonRes
		return nil
	}
}

var inferenceGraph *v1alpha1.InferenceGraphSpec

func graphHandler(w http.ResponseWriter, req *http.Request) {
	inputBytes, _ := ioutil.ReadAll(req.Body)
	res := make(chan []byte)
	rootNodes := []string{}
	for name, _ := range inferenceGraph.Nodes {
		rootNodes = append(rootNodes, name)
	}
	go func() {
		err := routeStep(v1alpha1.GraphRootNodeName, inferenceGraph.Nodes[v1alpha1.GraphRootNodeName], *inferenceGraph, inputBytes, res)
		if err != nil {
			log.Error(err, "failed to process request")
		}
	}()
	response := <-res
	w.Write(response)
}

var (
	jsonGraph = flag.String("graph-json", "", "serialized json graph def")
)

func main() {
	flag.Parse()
	logf.SetLogger(zap.New())
	inferenceGraph = &v1alpha1.InferenceGraphSpec{}
	err := json.Unmarshal([]byte(*jsonGraph), inferenceGraph)
	if err != nil {
		log.Error(err, "failed to unmarshall inference graph json")
		os.Exit(1)
	}

	http.HandleFunc("/", graphHandler)

	err = http.ListenAndServe(":8080", nil)
	if err != nil {
		log.Error(err, "failed to listen on 8080")
		os.Exit(1)
	}
}
