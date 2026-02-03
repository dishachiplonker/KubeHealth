package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"
	"os/signal"
	"syscall"
	"k8s.io/apimachinery/pkg/watch"
	"net/http"
	
    corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

func getenv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func buildConfig() (*rest.Config, string) {
	// 1) In-cluster config (what you'll use when running inside Kubernetes)
	cfg, err := rest.InClusterConfig()
	if err == nil {
		return cfg, "in-cluster"
	}

	// 2) Local kubeconfig (for local dev/testing)
	kubeconfig := os.Getenv("KUBECONFIG")
	if kubeconfig == "" {
		home, _ := os.UserHomeDir()
		kubeconfig = filepath.Join(home, ".kube", "config")
	}

	cfg, err = clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		log.Fatalf("failed to build kubeconfig: %v", err)
	}
	return cfg, "kubeconfig"
}

func isCrashLoopBackOff(pod corev1.Pod) bool {
	for _, cs := range pod.Status.ContainerStatuses {
		if cs.State.Waiting != nil && cs.State.Waiting.Reason == "CrashLoopBackOff" {
			return true
		}
	}
	return false
}

func podKey(p corev1.Pod) string {
	return p.Namespace + "/" + p.Name
}

func deletePod(ctx context.Context, clientset *kubernetes.Clientset, ns, podName string) error {
	return clientset.CoreV1().Pods(ns).Delete(ctx, podName, metav1.DeleteOptions{})
}

func healthzHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

func main() {
	ns := getenv("NAMESPACE", "default")

	cfg, mode := buildConfig()
	clientset, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		log.Fatalf("failed to create clientset: %v", err)
	}

	// NEW: run until Ctrl+C / SIGTERM instead of timing out
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	port := getenv("PORT", "8080")

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", healthzHandler)

	srv := &http.Server{
		Addr:    ":" + port,
		Handler: mux,
	}

	go func() {
		fmt.Printf("health server listening on :%s\n", port)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("health server error: %v", err)
		}
	}()


	fmt.Printf("connected via %s\n", mode)
	fmt.Printf("watching namespace: %s\n", ns)

	w, err := clientset.CoreV1().Pods(ns).Watch(ctx, metav1.ListOptions{})
	if err != nil {
		log.Fatalf("failed to start pod watch: %v", err)
	}
	defer w.Stop()

	// Track pods we have already reported as CrashLoopBackOff to reduce spam.
	reported := make(map[string]bool)

	for {
		select {
		case <-ctx.Done():
			fmt.Println("shutting down...")

			shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = srv.Shutdown(shutdownCtx)

			fmt.Println("shutdown complete")
			return

		case evt, ok := <-w.ResultChan():
			if !ok {
				fmt.Println("watch channel closed")
				return
			}

			pod, ok := evt.Object.(*corev1.Pod)
			if !ok || pod == nil {
				continue
			}

			if evt.Type == watch.Deleted {
				delete(reported, pod.Namespace+"/"+pod.Name)
				continue
			}

			key := pod.Namespace + "/" + pod.Name

			if isCrashLoopBackOff(*pod) {
				if !reported[key] {
					reported[key] = true
			
					ts := time.Now().Format(time.RFC3339)
					reason := "CrashLoopBackOff"
					fmt.Printf("%s restarted pod %s due to %s\n", ts, key, reason)
			
					delCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
					err := deletePod(delCtx, clientset, pod.Namespace, pod.Name)
					cancel()
			
					if err != nil {
						fmt.Printf("%s failed to delete pod %s: %v\n", ts, key, err)
					} else {
						fmt.Printf("%s deleted pod %s\n", ts, key)
					}
				}
			} else {
				if reported[key] {
					delete(reported, key)
				}
			}
		}
	}
}

