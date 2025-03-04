package main

import (
	"context"
	"encoding/json"
	_ "expvar"
	"flag"
	"fmt"
	"net/http"
	"strings"

	"github.com/cyverse-de/configurate"
	"github.com/cyverse-de/go-mod/otelutils"
	"github.com/cyverse-de/model/v6"
	"github.com/spf13/viper"

	"github.com/gorilla/mux"
	"github.com/sirupsen/logrus"

	"github.com/cyverse-de/messaging/v9"

	"go.opentelemetry.io/contrib/instrumentation/github.com/gorilla/mux/otelmux"
)

const serviceName = "job-status-listener"

var log = logrus.WithFields(logrus.Fields{
	"service": serviceName,
	"art-id":  serviceName,
	"group":   "org.cyverse",
})

var (
	cfgPath = flag.String("config", "", "Path to the configuration file.")
	cfg     *viper.Viper
)

func update(ctx context.Context, publisher JobUpdatePublisher, state messaging.JobState, jobID string, hostname string, msg string) (*messaging.UpdateMessage, error) {
	updateMessage := &messaging.UpdateMessage{
		Job:     &model.Job{InvocationID: jobID},
		State:   state,
		Message: msg,
		Sender:  hostname,
	}

	err := publisher.PublishJobUpdate(ctx, updateMessage)
	if err == nil {
		log.Infof("%s (%s) [%s]: %s", jobID, state, hostname, msg)
		return updateMessage, nil
	}

	// The first attempt to record the message failed.
	log.Errorf("failed to publish job status update: %s", err)

	// Attempt to reestablish the connection.
	log.Info("attempting to reestablish the messaging connection")
	err = publisher.Reconnect()
	if err != nil {
		log.Errorf("unable to reestablish the messaging connection: %s", err)
		return nil, err
	}

	// Attempt to record the message one more time.
	err = publisher.PublishJobUpdate(ctx, updateMessage)
	if err == nil {
		log.Infof("%s (%s) [%s]: %s", jobID, state, hostname, msg)
		return updateMessage, nil
	}

	log.Errorf("failed to publish job status update again - giving up: %s", err)
	return nil, err
}

// MessagePost describes the structure of the job status update request body.
type MessagePost struct {
	Hostname string
	Message  string
	State    string
}

// MessagePostWithUUIDs describes the job status update sent from the new batch
// analysis execution system. It differentiates between the job UUID and the analysis
// UUID and contains both in the body of the request rather than in the URL.
type MessagePostWithUUIDs struct {
	JobUUID  string `json:"job_uuid"`
	Hostname string
	Message  string
	State    string
}

func getState(state string) (messaging.JobState, error) {
	switch strings.ToLower(state) {
	case "submitted":
		return messaging.SubmittedState, nil
	case "running":
		return messaging.RunningState, nil
	case "completed":
		return messaging.SucceededState, nil
	case "succeeded":
		return messaging.SucceededState, nil
	case "failed":
		return messaging.FailedState, nil
	default:
		return "", fmt.Errorf("Unknown job state: %s", state)
	}
}

func postBatchStatus(publisher JobUpdatePublisher, w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	out := json.NewEncoder(w)

	var updateMessage MessagePostWithUUIDs

	err := json.NewDecoder(r.Body).Decode(&updateMessage)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		log.Error(err)
		_ = out.Encode(map[string]string{
			"error": err.Error(),
		})
		return
	}

	jobID := updateMessage.JobUUID

	state, err := getState(updateMessage.State)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		log.Error(err)
		_ = out.Encode(map[string]string{
			"error": err.Error(),
		})
		return
	}

	log.Infof("batch analysis => job id: '%s', state: '%s', hostname: '%s', message :'%s'",
		jobID,
		state,
		updateMessage.Hostname,
		updateMessage.Message,
	)

	msg, err := update(ctx, publisher, state, jobID, updateMessage.Hostname, updateMessage.Message)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		log.Error(err)
		_ = out.Encode(map[string]string{
			"error": err.Error(),
		})
		log.Fatal("failed to record a valid job status update - aborting")
	}
	_ = out.Encode(msg)
}

func postUpdate(publisher JobUpdatePublisher, w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	out := json.NewEncoder(w)

	var updateMessage MessagePost

	vars := mux.Vars(r)
	jobID := vars["uuid"]

	err := json.NewDecoder(r.Body).Decode(&updateMessage)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		log.Error(err)
		_ = out.Encode(map[string]string{
			"error": err.Error(),
		})
		return
	}

	state, err := getState(updateMessage.State)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		log.Error(err)
		_ = out.Encode(map[string]string{
			"error": err.Error(),
		})
		return
	}

	msg, err := update(ctx, publisher, state, jobID, updateMessage.Hostname, updateMessage.Message)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		log.Error(err)
		_ = out.Encode(map[string]string{
			"error": err.Error(),
		})
		log.Fatal("failed to record a valid job status update - aborting")
	}
	_ = out.Encode(msg)
}

func init() {
	flag.Parse()
	logrus.SetFormatter(&logrus.JSONFormatter{})
}

func loadConfig(cfgPath string) {
	var err error
	cfg, err = configurate.Init(cfgPath)
	if err != nil {
		log.Fatal(err)
	}
}

func newRouter(publisher JobUpdatePublisher) *mux.Router {
	r := mux.NewRouter()
	r.Use(otelmux.Middleware(serviceName))
	r.Handle("/debug/vars", http.DefaultServeMux)
	r.Path("/{uuid:[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}}/status").Methods("POST").HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			postUpdate(publisher, w, r)
		},
	)
	r.Path("/status/batch").Methods("POST").HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			postBatchStatus(publisher, w, r)
		},
	)

	return r
}

func main() {
	log.Info("Starting up the job-status-listener service.")

	var tracerCtx, cancel = context.WithCancel(context.Background())
	defer cancel()
	shutdown := otelutils.TracerProviderFromEnv(tracerCtx, serviceName, func(e error) { log.Fatal(e) })
	defer shutdown()

	loadConfig(*cfgPath)

	uri := cfg.GetString("amqp.uri")
	exchange := cfg.GetString("amqp.exchange.name")

	publisher, err := NewDefaultJobUpdatePublisher(uri, exchange)
	if err != nil {
		log.Fatal(err)
	}
	defer publisher.Close()

	r := newRouter(publisher)

	listenPortSpec := ":" + "60000"
	log.Infof("Listening on %s", listenPortSpec)
	log.Fatal(http.ListenAndServe(listenPortSpec, r))
}
