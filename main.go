package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/informers"
	"k8s.io/apimachinery/pkg/util/retry"
	"k8s.io/apimachinery/pkg/types"
)

func main() {
	config, err := rest.InClusterConfig()
	if err != nil {
		panic(err.Error())
	}
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		panic(err.Error())
	}

	factory := informers.NewSharedInformerFactory(clientset, time.Minute*10)
	secretInformer := factory.Core().V1().Secrets().Informer()

	stopCh := make(chan struct{})
	defer close(stopCh)

	secretInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    handleSecret,
		UpdateFunc: func(oldObj, newObj interface{}) { handleSecret(newObj) },
	})

	factory.Start(stopCh)

	<-stopCh
}

func handleSecret(obj interface{}) {
	secret, ok := obj.(*metav1.Secret)
	if !ok {
		fmt.Println("could not parse secret")
		return
	}

	expiryAnnotation := secret.Annotations["secret-watcher.expiry"]
	if expiryAnnotation == "" {
		return
	}

	expiryTime, err := parseExpiry(expiryAnnotation)
	if err != nil {
		fmt.Printf("could not parse expiry: %v\n", err)
		return
	}

	if time.Now().After(expiryTime) {
		fmt.Printf("Secret %s/%s expired!\n", secret.Namespace, secret.Name)
		clientset, _ := kubernetes.NewForConfigOrDie(rest.InClusterConfig())

		// Find all pods using this secret
		pods, err := clientset.CoreV1().Pods(secret.Namespace).List(context.Background(), metav1.ListOptions{})
		if err != nil {
			fmt.Printf("error listing pods: %v\n", err)
			return
		}

		for _, pod := range pods.Items {
			for _, volume := range pod.Spec.Volumes {
				if volume.Secret != nil && volume.Secret.SecretName == secret.Name {
					// Restart the deployment
					restartPodOwner(clientset, &pod)
					break
				}
			}
		}
	}
}

func parseExpiry(annotation string) (time.Time, error) {
	// Try RFC3339
	if t, err := time.Parse(time.RFC3339, annotation); err == nil {
		return t, nil
	}
	// Try UNIX timestamp (seconds)
	if seconds, err := time.ParseDuration(annotation + "s"); err == nil {
		return time.Now().Add(seconds * -1), nil
	}
	return time.Time{}, fmt.Errorf("unsupported expiry format")
}

func restartPodOwner(clientset *kubernetes.Clientset, pod *metav1.Pod) {
	for _, owner := range pod.OwnerReferences {
		if *owner.Controller && strings.ToLower(owner.Kind) == "replicaset" {
			rs, err := clientset.AppsV1().ReplicaSets(pod.Namespace).Get(context.Background(), owner.Name, metav1.GetOptions{})
			if err != nil {
				fmt.Printf("error getting replicaset: %v\n", err)
				continue
			}

			depName := rs.OwnerReferences[0].Name
			fmt.Printf("Restarting Deployment %s for pod %s\n", depName, pod.Name)
			err = rolloutRestartDeployment(clientset, pod.Namespace, depName)
			if err != nil {
				fmt.Printf("failed to restart deployment: %v\n", err)
			}
		}
	}
}

func rolloutRestartDeployment(clientset *kubernetes.Clientset, namespace, deploymentName string) error {
	depClient := clientset.AppsV1().Deployments(namespace)
	ctx := context.Background()

	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		dep, err := depClient.Get(ctx, deploymentName, metav1.GetOptions{})
		if err != nil {
			return err
		}

		if dep.Spec.Template.Annotations == nil {
			dep.Spec.Template.Annotations = map[string]string{}
		}
		dep.Spec.Template.Annotations["kubectl.kubernetes.io/restartedAt"] = time.Now().Format(time.RFC3339)

		_, err = depClient.Update(ctx, dep, metav1.UpdateOptions{})
		return err
	})
}
