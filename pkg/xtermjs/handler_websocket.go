package xtermjs

import (
	"cloudshell/internal/log"
	"context"
	"crypto/tls"
	"fmt"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const DefaultConnectionErrorLimit = 10

type HandlerOpts struct {
	// AllowedHostnames is a list of strings which will be matched to the client
	// requesting for a connection upgrade to a websocket connection
	AllowedHostnames []string
	// Arguments is a list of strings to pass as arguments to the specified Command
	Arguments []string
	// Command is the path to the binary we should create a TTY for
	Command string
	// ConnectionErrorLimit defines the number of consecutive errors that can happen
	// before a connection is considered unusable
	ConnectionErrorLimit int
	// CreateLogger when specified should return a logger that the handler will use.
	// The string argument being passed in will be a unique identifier for the
	// current connection. When not specified, logs will be sent to stdout
	CreateLogger func(string, *http.Request) Logger
	// KeepalivePingTimeout defines the maximum duration between which a ping and pong
	// cycle should be tolerated, beyond this the connection should be deemed dead
	KeepalivePingTimeout time.Duration
	MaxBufferSizeBytes   int
	// Kubernetes
	KubernetesHost      string
	KubernetesNamespace string
	PodName             string
	KubernetesToken     string
}

func GetHandler(opts HandlerOpts) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		// Initialize connection parameters
		connectionErrorLimit := opts.ConnectionErrorLimit
		if connectionErrorLimit < 0 {
			connectionErrorLimit = DefaultConnectionErrorLimit
		}
		maxBufferSizeBytes := opts.MaxBufferSizeBytes
		keepalivePingTimeout := opts.KeepalivePingTimeout
		if keepalivePingTimeout <= time.Second {
			keepalivePingTimeout = 20 * time.Second
		}

		// Create connection UUID and logger
		connectionUUID, err := uuid.NewUUID()
		if err != nil {
			message := "failed to get a connection uuid"
			log.Errorf("%s: %s", message, err)
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte(message))
			return
		}
		var clog Logger = defaultLogger
		if opts.CreateLogger != nil {
			clog = opts.CreateLogger(connectionUUID.String(), r)
		}
		clog.Info("established connection identity")

		// Upgrade to websocket connection
		upgrader := getConnectionUpgrader(opts.AllowedHostnames, maxBufferSizeBytes, clog)
		connection, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			clog.Warnf("failed to upgrade connection: %s", err)
			return
		}
		defer connection.Close()

		// Create main context for the connection
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		// Generate pod name
		podName := fmt.Sprintf("xtermjs-%s", connectionUUID.String())

		// Create Kubernetes config
		config, err := rest.InClusterConfig()
		if err != nil {
			config = &rest.Config{
				Host: strings.TrimPrefix(opts.KubernetesHost, "https://"),
				TLSClientConfig: rest.TLSClientConfig{
					Insecure: true,
				},
				BearerToken: opts.KubernetesToken,
			}
		} else {
			config.BearerToken = opts.KubernetesToken
			config.Host = strings.TrimPrefix(opts.KubernetesHost, "https://")
			config.TLSClientConfig.Insecure = true
		}

		// Create Kubernetes clientset
		clientset, err := kubernetes.NewForConfig(config)
		if err != nil {
			clog.Warnf("failed to create clientset: %s", err)
			return
		}

		// Define container spec
		container := corev1.Container{
			Name:    "shell",
			Image:   "alpine",
			Command: []string{"tail", "-f", "/dev/null"},
			Stdin:   true,
			TTY:     true,
			Resources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("100m"),
					corev1.ResourceMemory: resource.MustParse("64Mi"),
				},
				Limits: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("500m"),
					corev1.ResourceMemory: resource.MustParse("256Mi"),
				},
			},
			ImagePullPolicy: corev1.PullIfNotPresent,
		}

		// Define pod spec
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      podName,
				Namespace: opts.KubernetesNamespace,
			},
			Spec: corev1.PodSpec{
				Containers:    []corev1.Container{container},
				RestartPolicy: corev1.RestartPolicyAlways,
				SecurityContext: &corev1.PodSecurityContext{
					RunAsUser:  int64Ptr(1000),
					RunAsGroup: int64Ptr(1000),
				},
			},
		}

		clog.Infof("Creating Pod %s in namespace %s", podName, opts.KubernetesNamespace)

		// Create pod with timeout
		createCtx, createCancel := context.WithTimeout(ctx, 30*time.Second)
		defer createCancel()
		_, err = clientset.CoreV1().Pods(opts.KubernetesNamespace).Create(createCtx, pod, metav1.CreateOptions{})
		if err != nil {
			clog.Errorf("Failed to create Pod: %s", err)
			if statusErr, ok := err.(*errors.StatusError); ok {
				clog.Errorf("Status: %v", statusErr.Status())
			}
			return
		}

		// Setup pod deletion (deferred to ensure cleanup)
		defer func() {
			clog.Info("Initiating pod cleanup...")
			deleteCtx, deleteCancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer deleteCancel()
			err := clientset.CoreV1().Pods(opts.KubernetesNamespace).Delete(deleteCtx, podName, metav1.DeleteOptions{})
			if err != nil {
				if errors.IsNotFound(err) {
					clog.Info("Pod already deleted")
				} else {
					clog.Warnf("Failed to delete pod: %s", err)
				}
			} else {
				clog.Info("Pod deleted successfully")
			}
		}()

		// Wait for pod to be ready
		clog.Info("Waiting for pod to be ready...")
		if err := waitForPodReady(ctx, clientset, opts.KubernetesNamespace, podName); err != nil {
			clog.Warnf("Pod never became ready: %s", err)
			return
		}

		// Create websocket connection to pod
		podURL := fmt.Sprintf("wss://%s/api/v1/namespaces/%s/pods/%s/exec?command=%s&stdin=true&stdout=true&stderr=true&tty=true",
			opts.KubernetesHost, opts.KubernetesNamespace, podName, url.QueryEscape("sh"))

		clog.Debugf("Connecting to pod at: %s", podURL)

		headers := http.Header{
			"Authorization": {"Bearer " + opts.KubernetesToken},
		}

		dialer := websocket.Dialer{
			TLSClientConfig: &tls.Config{
				InsecureSkipVerify: true,
			},
		}

		ttyConn, _, err := dialer.Dial(podURL, headers)
		if err != nil {
			clog.Warnf("Failed to connect to pod: %s", err)
			return
		}
		defer ttyConn.Close()

		// Connection management
		var connectionClosed bool
		var waiter sync.WaitGroup
		waiter.Add(1)

		// Ping-pong keepalive
		lastPongTime := time.Now()
		connection.SetPongHandler(func(msg string) error {
			lastPongTime = time.Now()
			return nil
		})

		// Goroutine for sending periodic pings
		go func() {
			ticker := time.NewTicker(keepalivePingTimeout / 2)
			defer ticker.Stop()

			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					if time.Since(lastPongTime) > keepalivePingTimeout {
						clog.Warn("Ping timeout - terminating connection")
						cancel()
						waiter.Done()
						return
					}
					if err := connection.WriteMessage(websocket.PingMessage, []byte("keepalive")); err != nil {
						clog.Warn("Failed to send ping - terminating connection")
						cancel()
						waiter.Done()
						return
					}
				}
			}
		}()

		// Goroutine for reading from pod and writing to client
		go func() {
			defer cancel()
			errorCounter := 0

			for {
				select {
				case <-ctx.Done():
					return
				default:
					if errorCounter > connectionErrorLimit {
						clog.Warn("Error limit exceeded - terminating connection")
						waiter.Done()
						return
					}

					messageType, message, err := ttyConn.ReadMessage()
					if err != nil {
						clog.Warnf("Failed to read from pod: %s", err)
						waiter.Done()
						return
					}

					if err := connection.WriteMessage(messageType, message); err != nil {
						clog.Warnf("Failed to write to client: %s", err)
						errorCounter++
						continue
					}

					errorCounter = 0
				}
			}
		}()

		// Goroutine for reading from client and writing to pod
		go func() {
			defer cancel()

			for {
				select {
				case <-ctx.Done():
					return
				default:
					messageType, data, err := connection.ReadMessage()
					if err != nil {
						if !connectionClosed {
							clog.Warnf("Failed to read from client: %s", err)
						}
						return
					}

					// Skip resize messages
					if messageType == websocket.BinaryMessage && len(data) > 0 && data[0] == 1 {
						continue
					}

					// Prepend channel byte (0 for stdin) and write to pod
					message := append([]byte{0}, data...)
					if err := ttyConn.WriteMessage(messageType, message); err != nil {
						clog.Warnf("Failed to write to pod: %s", err)
						continue
					}
				}
			}
		}()

		// Wait for termination
		waiter.Wait()
		clog.Info("Terminating connection...")
		connectionClosed = true
		cancel()
	}
}

func waitForPodReady(ctx context.Context, clientset *kubernetes.Clientset, namespace, podName string) error {
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			pod, err := clientset.CoreV1().Pods(namespace).Get(ctx, podName, metav1.GetOptions{})
			if err != nil {
				return err
			}

			switch pod.Status.Phase {
			case corev1.PodRunning:
				for _, cond := range pod.Status.Conditions {
					if cond.Type == corev1.PodReady && cond.Status == corev1.ConditionTrue {
						return nil
					}
				}
			case corev1.PodFailed, corev1.PodSucceeded:
				return fmt.Errorf("pod terminated unexpectedly with phase: %s", pod.Status.Phase)
			}
		}
	}
}

func int64Ptr(i int64) *int64 { return &i }
