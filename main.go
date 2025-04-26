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
	"k8s.io/client-go/util/retry"
)

const (
	ExpiryAnnotationKey = "secret-expiry" // Customize as needed
	PollInterval        = 5 * time.Minute // Polling interval
)

func main() {
	clientset, err := getClient()
	if err != nil {
		panic(err)
	}

	ticker := time.NewTicker(PollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			err := checkSecrets(clientset)
			if err != nil {
				fmt.Println("Error checking secrets:", err)
			}
		}
	}
}

// Get Kubernetes clientset
func getClient() (*kubernetes.Clientset, error) {
	// Inside cluster config
	config, err := rest.InClusterConfig()
	if err != nil {
		// fallback to kubeconfig (for local debug)
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

// Main secret checker
func checkSecrets(clientset *kubernetes.Clientset) error {
	secrets, err := clientset.CoreV1().Secrets("").List(context.Background(), metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("failed to list secrets: %v", err)
	}

	now := time.Now()

	for _, secret := range secrets.Items {
		expiryRaw, ok := secret.Annotations[ExpiryAnnotationKey]
		if !ok {
			continue
		}

		expired, err := isSecretExpired(secret, expiryRaw, now)
		if err != nil {
			fmt.Printf("Invalid expiry for secret %s/%s: %v\n", secret.Namespace, secret.Name, err)
			continue
		}

		if expired {
			fmt.Printf("Secret expired: %s/%s\n", secret.Namespace, secret.Name)
			err := handleExpiredSecret(clientset, secret)
			if err != nil {
				fmt.Printf("Error handling expired secret %s/%s: %v\n", secret.Namespace, secret.Name, err)
			}
		}
	}

	return nil
}

// Expiry checker
func isSecretExpired(secret v1.Secret, expiryRaw string, now time.Time) (bool, error) {
	// Try parse as absolute time (preferred)
	layouts := []string{time.RFC3339, "2006-01-02T15:04:05", "2006-01-02"}
	for _, layout := range layouts {
		if t, err := time.Parse(layout, expiryRaw); err == nil {
			return now.After(t), nil
		}
	}

	// Try parse as relative seconds since creation
	if secs, err := time.ParseDuration(expiryRaw + "s"); err == nil {
		created := secret.GetCreationTimestamp().Time
		expiry := created.Add(secs)
		return now.After(expiry), nil
	}

	return false, fmt.Errorf("unknown expiry format: %s", expiryRaw)
}

// Handle expired secret
func handleExpiredSecret(clientset *kubernetes.Clientset, secret v1.Secret) error {
	pods, err := clientset.CoreV1().Pods(secret.Namespace).List(context.Background(), metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("error listing pods: %v", err)
	}

	affectedDeployments := map[string]bool{}

	for _, pod := range pods.Items {
		if podUsesSecret(&pod, secret.Name) {
			for _, owner := range pod.OwnerReferences {
				if owner.Kind == "ReplicaSet" && owner.Controller != nil && *owner.Controller {
					rsName := owner.Name
					deploymentName := extractDeploymentName(rsName)
					if deploymentName != "" {
						affectedDeployments[deploymentName] = true
					}
				}
			}
		}
	}

	for dep := range affectedDeployments {
		fmt.Printf("Restarting deployment %s/%s\n", secret.Namespace, dep)
		err := rolloutRestartDeployment(clientset, secret.Namespace, dep)
		if err != nil {
			fmt.Printf("Failed to restart %s/%s: %v\n", secret.Namespace, dep, err)
		}
	}

	return nil
}

// Detect if a pod uses a secret
func podUsesSecret(pod *v1.Pod, secretName string) bool {
	for _, vol := range pod.Spec.Volumes {
		if vol.Secret != nil && vol.Secret.SecretName == secretName {
			return true
		}
	}
	return false
}

// Extract Deployment name from ReplicaSet name
func extractDeploymentName(rsName string) string {
	parts := strings.Split(rsName, "-")
	if len(parts) < 2 {
		return ""
	}
	return strings.Join(parts[:len(parts)-1], "-")
}

// Rollout restart a deployment
func rolloutRestartDeployment(clientset *kubernetes.Clientset, namespace, deploymentName string) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		depClient := clientset.AppsV1().Deployments(namespace)
		dep, err := depClient.Get(context.Background(), deploymentName, metav1.GetOptions{})
		if err != nil {
			return err
		}

		if dep.Spec.Template.Annotations == nil {
			dep.Spec.Template.Annotations = map[string]string{}
		}
		dep.Spec.Template.Annotations["kubectl.kubernetes.io/restartedAt"] = time.Now().Format(time.RFC3339)

		_, err = depClient.Update(context.Background(), dep, metav1.UpdateOptions{})
		return err
	})
}
