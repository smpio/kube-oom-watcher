package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"math/rand"
	"net/http"
	"regexp"
	"strconv"
	"time"

	v1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"

	apierrs "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"

	_ "github.com/lib/pq"
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

var (
	minWatchTimeout = 5 * time.Minute
	uidIndex        *map[types.UID]PodInfo
	pidRegExp       *regexp.Regexp
	cgroupRegExp    *regexp.Regexp
	webhookURL      string
	db              *sql.DB
)

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

	db, err = sql.Open("postgres", *dbURL)
	if err != nil {
		log.Fatalln(err)
	}
	defer db.Close()

	pidRegExp, err = regexp.Compile("Kill\\s+process\\s+(\\d+)")
	if err != nil {
		log.Fatalln(err)
	}

	cgroupRegExp, err = regexp.Compile("/pod([\\w\\-]+)/")
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
			if err != nil {
				err = handleError(err)
			}
		} else {
			err = handleError(oomEvent.Error)
		}

		if err != nil {
			log.Println(err)
		}
	}
}

func podIndexer(clientset *kubernetes.Clientset) {
	for {
		err := internalPodIndexer(clientset)
		if statusErr, ok := err.(*apierrs.StatusError); ok {
			if statusErr.ErrStatus.Reason == metav1.StatusReasonExpired {
				log.Println("podIndexer:", err, "Restarting watch")
				continue
			}
		}

		log.Fatalln(err)
	}
}

func internalPodIndexer(clientset *kubernetes.Clientset) error {
	list, err := clientset.CoreV1().Pods("").List(context.TODO(), metav1.ListOptions{})
	if err != nil {
		log.Fatalln(err)
	}

	index := make(map[types.UID]PodInfo, 1000)

	for _, pod := range list.Items {
		index[pod.UID] = PodInfo{
			Name:      pod.Name,
			Namespace: pod.Namespace,
		}
	}

	resourceVersion := list.ResourceVersion
	uidIndex = &index

	for {
		log.Println("podIndexer: watching since", resourceVersion)

		timeoutSeconds := int64(minWatchTimeout.Seconds() * (rand.Float64() + 1.0))
		watcher, err := clientset.CoreV1().Pods("").Watch(context.TODO(), metav1.ListOptions{
			ResourceVersion: resourceVersion,
			TimeoutSeconds:  &timeoutSeconds,
		})
		if err != nil {
			return err
		}

		for watchEvent := range watcher.ResultChan() {
			if watchEvent.Type == watch.Error {
				return apierrs.FromObject(watchEvent.Object)
			}

			pod, ok := watchEvent.Object.(*v1.Pod)
			if !ok {
				log.Println("podIndexer: unexpected kind:", watchEvent.Object.GetObjectKind().GroupVersionKind())
				continue
			}

			resourceVersion = pod.ResourceVersion

			if watchEvent.Type == watch.Added {
				index[pod.UID] = PodInfo{
					Name:      pod.Name,
					Namespace: pod.Namespace,
				}
			} else if watchEvent.Type == watch.Deleted {
				delete(index, pod.UID)
			}
		}
	}
}

func eventWatcher(clientset *kubernetes.Clientset, c chan OOMEvent) {
	for {
		err := internalEventWatcher(clientset, c)
		if statusErr, ok := err.(*apierrs.StatusError); ok {
			if statusErr.ErrStatus.Reason == metav1.StatusReasonExpired {
				log.Println("eventWatcher:", err, "Restarting watch")
				continue
			}
		}

		log.Fatalln(err)
	}
}

func internalEventWatcher(clientset *kubernetes.Clientset, c chan OOMEvent) error {
	list, err := clientset.CoreV1().Events("").List(context.TODO(), metav1.ListOptions{})
	if err != nil {
		return err
	}

	resourceVersion := list.ResourceVersion

	for {
		log.Println("eventWatcher: watching since", resourceVersion)

		timeoutSeconds := int64(minWatchTimeout.Seconds() * (rand.Float64() + 1.0))
		watcher, err := clientset.CoreV1().Events("").Watch(context.TODO(), metav1.ListOptions{
			ResourceVersion: resourceVersion,
			TimeoutSeconds:  &timeoutSeconds,
		})
		if err != nil {
			return err
		}

		for watchEvent := range watcher.ResultChan() {
			if watchEvent.Type == watch.Error {
				return apierrs.FromObject(watchEvent.Object)
			}

			event, ok := watchEvent.Object.(*v1.Event)
			if !ok {
				log.Println("eventWatcher: unexpected kind:", watchEvent.Object.GetObjectKind().GroupVersionKind())
				continue
			}

			resourceVersion = event.ResourceVersion

			if watchEvent.Type != watch.Added {
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

func extractUID(cgroup string) (types.UID, error) {
	match := cgroupRegExp.FindStringSubmatch(cgroup)
	if match == nil {
		return "", fmt.Errorf("Unknown cgroup format: %s", cgroup)
	}

	return types.UID(match[1]), nil
}

func handleOOM(event OOMEvent) error {
	var cgroup string
	var nspid uint64

	err := db.QueryRow(
		`SELECT cgroup, nspid
		FROM records
		WHERE
			hostname = $1 AND
			pid = $2 AND
			ts < current_timestamp
		ORDER BY ts DESC
		LIMIT 1`,
		event.Node, event.PID).Scan(&cgroup, &nspid)

	if err == sql.ErrNoRows {
		return fmt.Errorf("No ps record for node %s and PID %d", event.Node, event.PID)
	}

	if err != nil {
		return err
	}

	uid, err := extractUID(cgroup)
	if err != nil {
		return err
	}

	if uidIndex == nil {
		return fmt.Errorf("UID index not ready")
	}

	pod, ok := (*uidIndex)[uid]
	if !ok {
		return fmt.Errorf("Pod with UID %s is not known", uid)
	}

	return postMessage(map[string]string{
		"username": "OOM watcher",
		"text":     fmt.Sprintf("OOM in pod %s/%s (node: %s, PID: %d, NSPID: %d)", pod.Namespace, pod.Name, event.Node, event.PID, nspid),
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
