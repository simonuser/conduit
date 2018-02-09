package telemetry

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/golang/protobuf/ptypes/duration"
	"github.com/prometheus/client_golang/api"
	"github.com/prometheus/client_golang/api/prometheus/v1"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/common/model"
	common "github.com/runconduit/conduit/controller/gen/common"
	read "github.com/runconduit/conduit/controller/gen/controller/telemetry"
	write "github.com/runconduit/conduit/controller/gen/proxy/telemetry"
	public "github.com/runconduit/conduit/controller/gen/public"
	"github.com/runconduit/conduit/controller/k8s"
	"github.com/runconduit/conduit/controller/util"
	pkgK8s "github.com/runconduit/conduit/pkg/k8s"
	log "github.com/sirupsen/logrus"
	"golang.org/x/net/context"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	k8sV1 "k8s.io/api/core/v1"
)

var (
	requestLabels = []string{"source_deployment", "target_deployment", "method"}
	requestsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "requests_total",
			Help: "Total number of requests",
		},
		requestLabels,
	)

	responseLabels = append(requestLabels, []string{"http_status_code", "classification"}...)
	responsesTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "responses_total",
			Help: "Total number of responses",
		},
		responseLabels,
	)

	responseLatencyBuckets = append(append(append(append(append(
		prometheus.LinearBuckets(1, 1, 5),
		prometheus.LinearBuckets(10, 10, 5)...),
		prometheus.LinearBuckets(100, 100, 5)...),
		prometheus.LinearBuckets(1000, 1000, 5)...),
		prometheus.LinearBuckets(10000, 10000, 5)...),
	)

	responseLatency = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "response_latency_ms",
			Help:    "Response latency in milliseconds",
			Buckets: responseLatencyBuckets,
		},
		requestLabels,
	)
)

func init() {
	prometheus.MustRegister(requestsTotal)
	prometheus.MustRegister(responsesTotal)
	prometheus.MustRegister(responseLatency)
}

type (
	server struct {
		prometheusAPI     v1.API
		pods              *k8s.PodIndex
		replicaSets       *k8s.ReplicaSetStore
		instances         instanceCache
		ignoredNamespaces []string
	}

	instanceCache struct {
		sync.RWMutex
		cache map[string]time.Time
	}
)

func (c *instanceCache) update(id string) {
	c.Lock()
	defer c.Unlock()
	c.cache[id] = time.Now()
}

func (c *instanceCache) list() []string {
	c.RLock()
	defer c.RUnlock()

	instances := make([]string, 0)
	for name, _ := range c.cache {
		instances = append(instances, name)
	}
	return instances
}

func (c *instanceCache) purgeOldInstances() {
	c.Lock()
	defer c.Unlock()

	expiry := time.Now().Add(-10 * time.Minute)

	for name, time := range c.cache {
		if time.Before(expiry) {
			delete(c.cache, name)
		}
	}
}

func cleanupOldInstances(srv *server) {
	for _ = range time.Tick(10 * time.Second) {
		srv.instances.purgeOldInstances()
	}
}

func podIPKeyFunc(obj interface{}) ([]string, error) {
	if pod, ok := obj.(*k8sV1.Pod); ok {
		return []string{pod.Status.PodIP}, nil
	}
	return nil, fmt.Errorf("Object is not a Pod")
}

func NewServer(addr, prometheusUrl string, ignoredNamespaces []string, kubeconfig string) (*grpc.Server, net.Listener, error) {
	prometheusClient, err := api.NewClient(api.Config{Address: prometheusUrl})
	if err != nil {
		return nil, nil, err
	}

	clientSet, err := k8s.NewClientSet(kubeconfig)
	if err != nil {
		return nil, nil, err
	}

	pods, err := k8s.NewPodIndex(clientSet, podIPKeyFunc)
	if err != nil {
		return nil, nil, err
	}
	err = pods.Run()
	if err != nil {
		return nil, nil, err
	}

	replicaSets, err := k8s.NewReplicaSetStore(clientSet)
	if err != nil {
		return nil, nil, err
	}
	err = replicaSets.Run()
	if err != nil {
		return nil, nil, err
	}

	srv := &server{
		prometheusAPI:     v1.NewAPI(prometheusClient),
		pods:              pods,
		replicaSets:       replicaSets,
		instances:         instanceCache{cache: make(map[string]time.Time, 0)},
		ignoredNamespaces: ignoredNamespaces,
	}
	go cleanupOldInstances(srv)

	lis, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, nil, err
	}

	s := util.NewGrpcServer()
	read.RegisterTelemetryServer(s, srv)
	write.RegisterTelemetryServer(s, srv)

	// TODO: register shutdown hook to call pods.Stop() and replicatSets.Stop()

	return s, lis, nil
}

func (s *server) Query(ctx context.Context, req *read.QueryRequest) (*read.QueryResponse, error) {
	log.Debugf("Query request: %+v", req)

	samples := make([]*read.Sample, 0)

	if req.StartMs != 0 && req.EndMs != 0 && req.Step != "" {
		// timeseries query

		start := time.Unix(0, req.StartMs*int64(time.Millisecond))
		end := time.Unix(0, req.EndMs*int64(time.Millisecond))
		step, err := time.ParseDuration(req.Step)
		if err != nil {
			log.Errorf("ParseDuration(%+v) failed with: %+v", req.Step, err)
			return nil, err
		}

		queryRange := v1.Range{
			Start: start,
			End:   end,
			Step:  step,
		}

		res, err := s.prometheusAPI.QueryRange(ctx, req.Query, queryRange)
		if err != nil {
			log.Errorf("QueryRange(%+v, %+v) failed with: %+v", req.Query, queryRange, err)
			return nil, err
		}
		log.Debugf("Query response: %+v", res)

		if res.Type() != model.ValMatrix {
			err = fmt.Errorf("Unexpected query result type (expected Matrix): %s", res.Type())
			log.Errorf("%s", err)
			return nil, err
		}
		for _, s := range res.(model.Matrix) {
			samples = append(samples, convertSampleStream(s))
		}
	} else {
		// single data point (aka summary) query

		res, err := s.prometheusAPI.Query(ctx, req.Query, time.Time{})
		if err != nil {
			log.Errorf("Query(%+v, %+v) failed with: %+v", req.Query, time.Time{}, err)
			return nil, err
		}
		log.Debugf("Query response: %+v", res)

		if res.Type() != model.ValVector {
			err = fmt.Errorf("Unexpected query result type (expected Vector): %s", res.Type())
			log.Errorf("%s", err)
			return nil, err
		}
		for _, s := range res.(model.Vector) {
			samples = append(samples, convertSample(s))
		}
	}

	return &read.QueryResponse{Metrics: samples}, nil
}

func (s *server) ListPods(ctx context.Context, req *read.ListPodsRequest) (*public.ListPodsResponse, error) {
	log.Debugf("ListPods request: %+v", req)

	pods, err := s.pods.List()
	if err != nil {
		return nil, err
	}

	podList := make([]*public.Pod, 0)

	for _, pod := range pods {
		if s.shouldIngore(pod) {
			continue
		}
		deployment, err := s.replicaSets.GetDeploymentForPod(pod)
		if err != nil {
			log.Debugf("Cannot get deployment for pod %s: %s", pod.Name, err)
			deployment = ""
		}
		name := pod.Namespace + "/" + pod.Name
		updated, added := s.instances.cache[name]

		status := string(pod.Status.Phase)
		if pod.DeletionTimestamp != nil {
			status = "Terminating"
		}

		controllerComponent := pod.Labels[pkgK8s.ControllerComponentLabel]
		controllerNS := pod.Labels[pkgK8s.ControllerNSLabel]

		item := &public.Pod{
			Name:                pod.Namespace + "/" + pod.Name,
			Deployment:          deployment,
			Status:              status,
			PodIP:               pod.Status.PodIP,
			Added:               added,
			ControllerNamespace: controllerNS,
			ControlPlane:        controllerComponent != "",
		}
		if added {
			since := time.Since(updated)
			item.SinceLastReport = &duration.Duration{
				Seconds: int64(since / time.Second),
				Nanos:   int32(since % time.Second),
			}
		}
		podList = append(podList, item)
	}

	return &public.ListPodsResponse{Pods: podList}, nil
}

func (s *server) Report(ctx context.Context, req *write.ReportRequest) (*write.ReportResponse, error) {
	log.Debugf("Report request: %+v", req)

	id := "unknown"
	if req.Process != nil {
		id = req.Process.ScheduledNamespace + "/" + req.Process.ScheduledInstance
	}

	logCtx := log.WithFields(log.Fields{"id": id})
	logCtx.Debugf("Received report with %d requests", len(req.Requests))

	s.instances.update(id)

	for _, requestScope := range req.Requests {
		if requestScope.Ctx == nil {
			return nil, errors.New("RequestCtx is required")
		}
		requestLabels := s.requestLabelsFor(requestScope)
		requestsTotal.With(requestLabels).Add(float64(requestScope.Count))
		latencyStat := responseLatency.With(requestLabels)

		for _, responseScope := range requestScope.Responses {
			if responseScope.Ctx == nil {
				return nil, errors.New("ResponseCtx is required")
			}

			// Validate this ResponseScope's latency histogram.
			numBuckets := len(responseScope.ResponseLatencyCounts)
			expectedNumBuckets := len(req.HistogramBucketBoundsTenthMs)
			if numBuckets != expectedNumBuckets {
				err := errors.New(
					"received report with incorrect number of latency buckets")
				logCtx.WithFields(log.Fields{
					"numBuckets": numBuckets,
					"expected":   expectedNumBuckets,
					"scope":      responseScope,
				}).WithError(err).Error()
				return nil, err
			}

			for bucketNum, count := range responseScope.ResponseLatencyCounts {
				// Look up the bucket max value corresponding to this position
				// in the report's latency histogram.
				latencyTenthsMs := req.HistogramBucketBoundsTenthMs[bucketNum]
				latencyMs := float64(latencyTenthsMs) / 10
				for i := uint32(0); i < count; i++ {
					// Then, report that latency value to Prometheus a number
					// of times equal to the count reported by the proxy.
					latencyStat.Observe(latencyMs)
				}

			}

			for _, eosScope := range responseScope.Ends {
				if eosScope.Ctx == nil {
					return nil, errors.New("EosCtx is required")
				}

				responseLabels := s.requestLabelsFor(requestScope)
				for k, v := range responseLabelsFor(responseScope, eosScope) {
					responseLabels[k] = v
				}

				responsesTotal.With(responseLabels).Add(float64(len(eosScope.Streams)))
			}
		}

	}
	return &write.ReportResponse{}, nil
}

func (s *server) shouldIngore(pod *k8sV1.Pod) bool {
	for _, namespace := range s.ignoredNamespaces {
		if pod.Namespace == namespace {
			return true
		}
	}
	return false
}

// getDeployment returns the name of the deployment associated with a pod.
// If the name of the deployment could not be found, then a message will be
// logged, and getDeployment will return an emtpy string.
func (s *server) getDeployment(ip *common.IPAddress) string {
	ipStr := util.IPToString(ip)
	pods, err := s.pods.GetPodsByIndex(ipStr)
	if err != nil {
		log.Debugf("Cannot get pod for IP %s: %s", ipStr, err)
		return ""
	}
	if len(pods) == 0 {
		log.Debugf("No pod exists for IP %s", ipStr)
		return ""
	}
	if len(pods) > 1 {
		log.Debugf("Multiple pods found for IP %s", ipStr)
		return ""
	}
	pod := pods[0]
	deployment, err := (*s.replicaSets).GetDeploymentForPod(pod)
	if err != nil {
		log.WithError(err).Debugf("Cannot get deployment for pod %s", pod.Name)
		return ""
	}
	return deployment
}

func methodString(method *common.HttpMethod) string {
	switch method.Type.(type) {
	case *common.HttpMethod_Registered_:
		return method.GetRegistered().String()
	case *common.HttpMethod_Unregistered:
		return method.GetUnregistered()
	}
	return ""
}

func metricToMap(metric model.Metric) map[string]string {
	labels := make(map[string]string)
	for k, v := range metric {
		labels[string(k)] = string(v)
	}
	return labels
}

func convertSampleStream(sample *model.SampleStream) *read.Sample {
	values := make([]*read.SampleValue, 0)
	for _, s := range sample.Values {
		v := read.SampleValue{
			Value:       float64(s.Value),
			TimestampMs: int64(s.Timestamp),
		}
		values = append(values, &v)
	}

	return &read.Sample{Values: values, Labels: metricToMap(sample.Metric)}
}

func convertSample(sample *model.Sample) *read.Sample {
	values := []*read.SampleValue{
		&read.SampleValue{
			Value:       float64(sample.Value),
			TimestampMs: int64(sample.Timestamp),
		},
	}

	return &read.Sample{Values: values, Labels: metricToMap(sample.Metric)}
}

func (s *server) requestLabelsFor(requestScope *write.RequestScope) prometheus.Labels {
	sourceDeployment := s.getDeployment(requestScope.Ctx.SourceIp)
	targetDeployment := s.getDeployment(requestScope.Ctx.TargetAddr.Ip)

	return prometheus.Labels{
		"source_deployment": sourceDeployment,
		"target_deployment": targetDeployment,
		"method":            methodString(requestScope.Ctx.Method),
	}
}

func responseLabelsFor(responseScope *write.ResponseScope, eosScope *write.EosScope) prometheus.Labels {
	httpStatusCode := strconv.Itoa(int(responseScope.Ctx.HttpStatusCode))
	classification := "failure"
	switch x := eosScope.Ctx.End.(type) {
	case *write.EosCtx_GrpcStatusCode:
		// The stream ended with a `grpc-status` trailer.
		// Classify based on the gRPC status code.
		if x.GrpcStatusCode == uint32(codes.OK) {
			classification = "success"
		}
	case *write.EosCtx_Other:
		// The stream did not end with a `grpc-status` trailer (i.e., it was
		// not a gRPC message). Classify based on the response's HTTP status.
		if responseScope.Ctx.HttpStatusCode < http.StatusInternalServerError {
			classification = "success"
		}
	}
	return prometheus.Labels{
		"http_status_code": httpStatusCode,
		"classification":   classification,
	}
}
