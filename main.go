package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"regexp"
	"strconv"

	v1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
)

// PodInfo contains pod info
type PodInfo struct {
	Name      string
	Namespace string
}

// OOMEvent comes from eventWatcher
type OOMEvent struct {
	Node  string
	PID   uint64
	Error error
}

var uidIndex map[types.UID]PodInfo
var pidRegExp *regexp.Regexp
var webhookURL string

func main() {
	masterURL := flag.String("master", "", "kubernetes api server url")
	kubeconfigPath := flag.String("kubeconfig", "", "path to kubeconfig file")
	dbURL := flag.String("db-url", "", "database URL")
	flag.StringVar(&webhookURL, "webhook-url", "", "webhook URL")
	flag.Parse()

	if *dbURL == "" {
		log.Fatalln("Database URL not set")
	}

	if webhookURL == "" {
		log.Fatalln("Webhook URL not set")
	}

	config, err := clientcmd.BuildConfigFromFlags(*masterURL, *kubeconfigPath)
	if err != nil {
		log.Fatalln(err)
	}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		log.Fatalln(err)
	}

	uidIndex = make(map[types.UID]PodInfo, 1000)

	pidRegExp, err = regexp.Compile("Kill\\s+process\\s+(\\d+)")
	if err != nil {
		log.Fatalln(err)
	}

	eventCh := make(chan OOMEvent, 128)

	go podIndexer(clientset)
	go eventWatcher(clientset, eventCh)

	for oomEvent := range eventCh {
		var err error
		if oomEvent.Error == nil {
			err = handleOOM(oomEvent)
		} else {
			err = handleError(oomEvent.Error)
		}

		if err != nil {
			log.Println(err)
		}
	}
}

func podIndexer(clientset *kubernetes.Clientset) {
	watcher, err := clientset.CoreV1().Pods("").Watch(metav1.ListOptions{})
	if err != nil {
		log.Fatalln(err)
	}

	for watchEvent := range watcher.ResultChan() {
		pod, ok := watchEvent.Object.(*v1.Pod)
		if !ok {
			log.Println("unexpected type:", watchEvent.Object.GetObjectKind().GroupVersionKind())
			continue
		}

		if watchEvent.Type == watch.Added {
			uidIndex[pod.UID] = PodInfo{
				Name:      pod.Name,
				Namespace: pod.Namespace,
			}
		} else if watchEvent.Type == watch.Deleted {
			delete(uidIndex, pod.UID)
		}
	}
}

func eventWatcher(clientset *kubernetes.Clientset, c chan OOMEvent) {
	watcher, err := clientset.CoreV1().Events("").Watch(metav1.ListOptions{})
	if err != nil {
		log.Fatalln(err)
	}

	for watchEvent := range watcher.ResultChan() {
		if watchEvent.Type != watch.Added {
			continue
		}

		event, ok := watchEvent.Object.(*v1.Event)
		if !ok {
			log.Println("unexpected type:", watchEvent.Object.GetObjectKind().GroupVersionKind())
			continue
		}

		if event.Reason != "OOMKilling" {
			continue
		}

		if event.InvolvedObject.Kind != "Node" {
			continue
		}

		node := event.InvolvedObject.Name
		pid, err := extractPID(event.Message)
		if err != nil {
			c <- OOMEvent{
				Error: err,
			}
		}

		c <- OOMEvent{
			Node: node,
			PID:  pid,
		}
	}
}

func extractPID(message string) (uint64, error) {
	match := pidRegExp.FindStringSubmatch(message)
	if match == nil {
		return 0, fmt.Errorf("Event message does not match: %s", message)
	}

	pid, err := strconv.ParseUint(match[1], 10, 64)
	if err != nil {
		return 0, err
	}

	return pid, nil
}

func handleOOM(event OOMEvent) error {
	return postMessage(map[string]string{
		"username": "OOM watcher",
		"text":     fmt.Sprintf("Node: %s, PID: %d", event.Node, event.PID),
	})
}

func handleError(err error) error {
	return postMessage(map[string]string{
		"username": "OOM watcher",
		"text":     fmt.Sprint("Error: ", err),
	})
}

func postMessage(data interface{}) error {
	jsonValue, err := json.Marshal(data)
	if err != nil {
		return err
	}

	resp, err := http.Post(webhookURL, "application/json", bytes.NewBuffer(jsonValue))
	if err != nil {
		return err
	}

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("Failed to POST: %s", resp.Status)
	}

	return nil
}
