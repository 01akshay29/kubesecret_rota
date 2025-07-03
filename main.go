package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	v1 "k8s.io/api/core/v1"
	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kubernetes "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

const (
	ExpiryAnnotationKey = "secret-expiry"
	PollInterval        = 15 * time.Second // shorter interval for easier testing
)

func main() {
	fmt.Println("Secret checker started...")

	clientset, err := getClient()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to create Kubernetes client: %v\n", err)
		os.Exit(1)
	}

	ticker := time.NewTicker(PollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			err := checkSecrets(clientset)
			if err != nil {
				fmt.Fprintf(os.Stderr, "Error checking secrets: %v\n", err)
			}
		}
	}
}

func getClient() (*kubernetes.Clientset, error) {
	config, err := rest.InClusterConfig()
	if err != nil {
		kubeconfig := os.Getenv("KUBECONFIG")
		if kubeconfig == "" {
			return nil, fmt.Errorf("cannot create in-cluster config and no KUBECONFIG provided")
		}
		config, err = clientcmd.BuildConfigFromFlags("", kubeconfig)
		if err != nil {
			return nil, err
		}
	}
	return kubernetes.NewForConfig(config)
}

func checkSecrets(clientset *kubernetes.Clientset) error {
	secrets, err := clientset.CoreV1().Secrets("").List(context.Background(), metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("failed to list secrets: %w", err)
	}

	for _, secret := range secrets.Items {
		expiryRaw, ok := secret.Annotations[ExpiryAnnotationKey]
		if !ok {
			continue
		}

		// Print in previous format
		fmt.Printf("Secret: %s/%s, Expiry: %s\n", secret.Namespace, secret.Name, expiryRaw)

		err := printResourcesUsingSecret(clientset, secret)
		if err != nil {
			fmt.Printf("  Error listing resources using secret: %v\n", err)
		}
		fmt.Println()
	}

	return nil
}

func printResourcesUsingSecret(clientset *kubernetes.Clientset, secret v1.Secret) error {
	deployments := map[string]bool{}
	replicaSets := map[string]bool{}
	daemonSets := map[string]bool{}

	pods, err := clientset.CoreV1().Pods(secret.Namespace).List(context.Background(), metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("error listing pods: %w", err)
	}

	for _, pod := range pods.Items {
		if podUsesSecret(&pod, secret.Name) {
			for _, owner := range pod.OwnerReferences {
				if owner.Controller == nil || !*owner.Controller {
					continue
				}

				switch owner.Kind {
				case "ReplicaSet":
					rsName := owner.Name
					replicaSets[rsName] = true
					depName := extractDeploymentName(rsName)
					if depName != "" {
						deployments[depName] = true
					}
				case "DaemonSet":
					daemonSets[owner.Name] = true
				case "Deployment":
					deployments[owner.Name] = true
				}
			}
		}
	}

	rsList, err := clientset.AppsV1().ReplicaSets(secret.Namespace).List(context.Background(), metav1.ListOptions{})
	if err == nil {
		for _, rs := range rsList.Items {
			var _ appsv1.ReplicaSet = rs
			if podTemplateUsesSecret(rs.Spec.Template, secret.Name) {
				replicaSets[rs.Name] = true
				depName := extractDeploymentName(rs.Name)
				if depName != "" {
					deployments[depName] = true
				}
			}
		}
	}

	dsList, err := clientset.AppsV1().DaemonSets(secret.Namespace).List(context.Background(), metav1.ListOptions{})
	if err == nil {
		for _, ds := range dsList.Items {
			var _ appsv1.DaemonSet = ds
			if podTemplateUsesSecret(ds.Spec.Template, secret.Name) {
				daemonSets[ds.Name] = true
			}
		}
	}

	if len(deployments) == 0 && len(replicaSets) == 0 && len(daemonSets) == 0 {
		fmt.Println("  No deployments, replicasets, or daemonsets use this secret.")
	} else {
		if len(deployments) > 0 {
			fmt.Println("  Used by Deployments:")
			for dep := range deployments {
				fmt.Printf("    - %s/%s\n", secret.Namespace, dep)
			}
		}
		if len(replicaSets) > 0 {
			fmt.Println("  Used by ReplicaSets:")
			for rs := range replicaSets {
				fmt.Printf("    - %s/%s\n", secret.Namespace, rs)
			}
		}
		if len(daemonSets) > 0 {
			fmt.Println("  Used by DaemonSets:")
			for ds := range daemonSets {
				fmt.Printf("    - %s/%s\n", secret.Namespace, ds)
			}
		}
	}

	return nil
}

func podUsesSecret(pod *v1.Pod, secretName string) bool {
	for _, vol := range pod.Spec.Volumes {
		if vol.Secret != nil && vol.Secret.SecretName == secretName {
			return true
		}
	}
	return false
}

func podTemplateUsesSecret(podTemplate v1.PodTemplateSpec, secretName string) bool {
	for _, vol := range podTemplate.Spec.Volumes {
		if vol.Secret != nil && vol.Secret.SecretName == secretName {
			return true
		}
	}
	return false
}

func extractDeploymentName(rsName string) string {
	parts := strings.Split(rsName, "-")
	if len(parts) < 2 {
		return ""
	}
	return strings.Join(parts[:len(parts)-1], "-")
}
