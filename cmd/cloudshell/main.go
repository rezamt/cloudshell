package main

import (
	"cloudshell/internal/log"
	"cloudshell/pkg/xtermjs"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path"
	"strings"
	"time"

	"github.com/gorilla/mux"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/spf13/cobra"
)

var VersionInfo string

func main() {
	if VersionInfo == "" {
		VersionInfo = "dev"
	}
	command := cobra.Command{
		Use:     "cloudshell",
		Short:   "Creates a web-based shell using xterm.js that links to an actual shell",
		Version: VersionInfo,
		RunE:    runE,
	}
	conf.ApplyToCobra(&command)
	command.Execute()
}

func runE(_ *cobra.Command, _ []string) error {
	// initialise the logger
	log.Init(log.Format(conf.GetString("log-format")), log.Level(conf.GetString("log-level")))

	// debug stuff
	command := conf.GetString("command")
	connectionErrorLimit := conf.GetInt("connection-error-limit")
	arguments := conf.GetStringSlice("arguments")
	allowedHostnames := conf.GetStringSlice("allowed-hostnames")
	keepalivePingTimeout := time.Duration(conf.GetInt("keepalive-ping-timeout")) * time.Second
	maxBufferSizeBytes := conf.GetInt("max-buffer-size-bytes")
	pathLiveness := conf.GetString("path-liveness")
	pathMetrics := conf.GetString("path-metrics")
	pathReadiness := conf.GetString("path-readiness")
	pathXTermJS := conf.GetString("path-xtermjs")
	serverAddress := conf.GetString("server-addr")
	serverPort := conf.GetInt("server-port")
	workingDirectory := conf.GetString("workdir")
	if !path.IsAbs(workingDirectory) {
		wd, err := os.Getwd()
		if err != nil {
			message := fmt.Sprintf("failed to get working directory: %s", err)
			log.Error(message)
			return errors.New(message)
		}
		workingDirectory = path.Join(wd, workingDirectory)
	}
	log.Infof("working directory     : '%s'", workingDirectory)
	log.Infof("command               : '%s'", command)
	log.Infof("arguments             : ['%s']", strings.Join(arguments, "', '"))

	log.Infof("allowed hosts         : ['%s']", strings.Join(allowedHostnames, "', '"))
	log.Infof("connection error limit: %v", connectionErrorLimit)
	log.Infof("keepalive ping timeout: %v", keepalivePingTimeout)
	log.Infof("max buffer size       : %v bytes", maxBufferSizeBytes)
	log.Infof("server address        : '%s' ", serverAddress)
	log.Infof("server port           : %v", serverPort)

	log.Infof("liveness checks path  : '%s'", pathLiveness)
	log.Infof("readiness checks path : '%s'", pathReadiness)
	log.Infof("metrics endpoint path : '%s'", pathMetrics)
	log.Infof("xtermjs endpoint path : '%s'", pathXTermJS)

	// configure routing
	router := mux.NewRouter()

	// this is the endpoint for xterm.js to connect to
	xtermjsHandlerOptions := xtermjs.HandlerOpts{
		AllowedHostnames:     allowedHostnames,
		Arguments:            arguments,
		Command:              command,
		ConnectionErrorLimit: connectionErrorLimit,
		CreateLogger: func(connectionUUID string, r *http.Request) xtermjs.Logger {
			createRequestLog(r, map[string]interface{}{"connection_uuid": connectionUUID}).Infof("created logger for connection '%s'", connectionUUID)
			return createRequestLog(nil, map[string]interface{}{"connection_uuid": connectionUUID})
		},
		KeepalivePingTimeout: keepalivePingTimeout,
		MaxBufferSizeBytes:   maxBufferSizeBytes,
		KubernetesHost:       "127.0.0.1:57918",
		KubernetesNamespace:  "xtermjs",
		KubernetesToken:      "eyJhbGciOiJSUzI1NiIsImtpZCI6IkZSU0g3TDljLXdhRGl0Ymwyd1BOckFCVkZ5cy00M3lwZjdYVDlmMUJrQmcifQ.eyJpc3MiOiJrdWJlcm5ldGVzL3NlcnZpY2VhY2NvdW50Iiwia3ViZXJuZXRlcy5pby9zZXJ2aWNlYWNjb3VudC9uYW1lc3BhY2UiOiJkZWZhdWx0Iiwia3ViZXJuZXRlcy5pby9zZXJ2aWNlYWNjb3VudC9zZWNyZXQubmFtZSI6Ims4cy1yb290LXRva2VuIiwia3ViZXJuZXRlcy5pby9zZXJ2aWNlYWNjb3VudC9zZXJ2aWNlLWFjY291bnQubmFtZSI6Ims4cy1yb290Iiwia3ViZXJuZXRlcy5pby9zZXJ2aWNlYWNjb3VudC9zZXJ2aWNlLWFjY291bnQudWlkIjoiY2MxNThiOGUtYzVlZC00ZGI3LTkyN2EtN2U2M2M5OTcxYjFlIiwic3ViIjoic3lzdGVtOnNlcnZpY2VhY2NvdW50OmRlZmF1bHQ6azhzLXJvb3QifQ.kf6hUILzGvn1ZfXOTNoK1gfUX74C589Gsb89iLvZRPn4eRDJl9qyr-PsnZTVhpTUJv6DBt_MvL7-nYkMu6EiJTqMYHSAeM39d9L_4lC7OmEHY9R60ncyntSfcoh-OiFwBKJ1JrMZ9jJbX84ytat9UoPDTFlucCEBB0WS8CDLFvkfMr3UzUtLjnYLIoqAPsv2HFwWZE8kkMvgIDf5BrIXeaHisDvJlgpIZ5a8Svg3TLP4HY1F0yVjgdFNZgXNb-45aj4UEcrI-ejxnWPy05kYAuDoDzVZwwM0emFN4Lr9mSuI0bEYImRogHk1n-NFZQOobuZIyPdiWAs2IuJQJ2b2dQ",
	}
	router.HandleFunc(pathXTermJS, xtermjs.GetHandler(xtermjsHandlerOptions))

	// readiness probe endpoint
	router.HandleFunc(pathReadiness, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	})

	// liveness probe endpoint
	router.HandleFunc(pathLiveness, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	})

	// metrics endpoint
	router.Handle(pathMetrics, promhttp.Handler())

	// version endpoint
	router.HandleFunc("/version", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(VersionInfo))
	})

	// this is the endpoint for serving xterm.js assets
	depenenciesDirectory := path.Join(workingDirectory, "./node_modules")
	router.PathPrefix("/assets").Handler(http.StripPrefix("/assets", http.FileServer(http.Dir(depenenciesDirectory))))

	// this is the endpoint for the root path aka website
	publicAssetsDirectory := path.Join(workingDirectory, "./public")
	router.PathPrefix("/").Handler(http.FileServer(http.Dir(publicAssetsDirectory)))

	// start memory logging pulse
	logWithMemory := createMemoryLog()
	go func(tick *time.Ticker) {
		for {
			logWithMemory.Debug("tick")
			<-tick.C
		}
	}(time.NewTicker(time.Second * 30))

	// listen
	listenOnAddress := fmt.Sprintf("%s:%v", serverAddress, serverPort)
	server := http.Server{
		Addr:    listenOnAddress,
		Handler: addIncomingRequestLogging(router),
	}

	log.Infof("starting server on interface:port '%s'...", listenOnAddress)
	return server.ListenAndServe()
}
