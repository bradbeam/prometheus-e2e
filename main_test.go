package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	common "github.com/prometheus/common/config"
	"github.com/prometheus/common/model"
	"github.com/prometheus/prometheus/config"
	"github.com/prometheus/prometheus/discovery"
	"github.com/prometheus/prometheus/discovery/targetgroup"
	"gopkg.in/yaml.v2"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/e2e-framework/klient/k8s"
	"sigs.k8s.io/e2e-framework/klient/k8s/resources"
	"sigs.k8s.io/e2e-framework/klient/wait"
	"sigs.k8s.io/e2e-framework/klient/wait/conditions"
	"sigs.k8s.io/e2e-framework/pkg/env"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/envfuncs"
	"sigs.k8s.io/e2e-framework/pkg/features"
)

var (
	testenv env.Environment
	scheme  = runtime.NewScheme()
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
}

func TestMain(m *testing.M) {
	testenv = env.New()

	kindClusterName := envconf.RandomName("my-cluster", 16)

	namespace := envconf.RandomName("sample-ns", 16)

	testenv.Setup(
		envfuncs.CreateKindCluster(kindClusterName),
		envfuncs.CreateNamespace(namespace),
	)

	testenv.Finish(
		envfuncs.DeleteNamespace(namespace),
		envfuncs.DestroyKindCluster(kindClusterName),
	)

	os.Exit(testenv.Run(m))
}

func TestPrometheus(t *testing.T) {
	f := features.New("prometheus")
	f.Setup(deployPrometheus())
	f.Assess("metrics remote written and filtered", assessPrometheus())
	f.Teardown(teardownPrometheus())

	testenv.Test(t, f.Feature())
}

func deployPrometheus() features.Func {
	return func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
		// Remote Write
		remoteWriteProgram, err := setupRemoteWrite()
		if err != nil {
			t.Fatal(err)
		}

		// cAdvisors
		cadvisorConfigs, cadvisorProgram, err := setupcAdvisors()
		if err != nil {
			t.Fatal(err)
		}

		numcAdvisors := len(cadvisorConfigs)

		// Prometheus server
		promDeployment, promConfigMap, err := setupPrometheus(numcAdvisors)

		objects := []k8s.Object{
			cadvisorProgram,
			remoteWriteProgram,
			promConfigMap,
			promDeployment,
		}

		for _, cm := range cadvisorConfigs {
			objects = append(objects, cm)
		}

		for _, object := range objects {
			object.SetNamespace(cfg.Namespace())

			if err := cfg.Client().Resources().Create(ctx, object); err != nil {
				t.Fatal(err)
			}
		}

		if err := waitForPrometheus(ctx, t, cfg, promDeployment, numcAdvisors); err != nil {
			t.Fatal(err)
		}

		return ctx
	}
}

func waitForPrometheus(ctx context.Context, t *testing.T, cfg *envconf.Config, promDeployment *appsv1.Deployment, numcAdvisors int) error {
	// Handy function to wait for pod to be running
	err := wait.For(
		conditions.New(cfg.Client().Resources()).
			DeploymentConditionMatch(promDeployment, appsv1.DeploymentAvailable, corev1.ConditionTrue),
		wait.WithTimeout(time.Minute*1),
		wait.WithInterval(100*time.Millisecond),
	)
	if err != nil {
		return err
	}

	// Get prometheus pod
	podList := &corev1.PodList{}
	err = cfg.Client().Resources().WithNamespace(cfg.Namespace()).List(ctx, podList, resources.WithLabelSelector("app=prometheus"))
	if err != nil {
		return err
	}

	if len(podList.Items) != 1 {
		return fmt.Errorf("expected 1 prometheus pod but got %d", len(podList.Items))
	}

	expectedUpDog := generateUpDog(numcAdvisors)

	// Wait for all of the cadvsiors scrapes to complete
	// // This should be enough to signify that remote write is complete as well
	err = wait.For(
		func() (bool, error) {
			// Going to ignore the errors here because we want to retry
			done, _ := execAndGetMetrics(ctx, cfg, podList.Items[0].Name, strings.NewReader(expectedUpDog), []string{"up"})
			return done, nil
		},
		wait.WithInterval(1*time.Second),
		wait.WithImmediate(),
	)
	if err != nil {
		return err
	}

	return nil
}

func assessPrometheus() features.Func {
	return func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
		// Get prometheus pod
		podList := &corev1.PodList{}
		err := cfg.Client().Resources().WithNamespace(cfg.Namespace()).List(ctx, podList, resources.WithLabelSelector("app=prometheus"))
		if err != nil {
			t.Fatal(err)
		}

		if len(podList.Items) != 1 {
			t.Fatalf("expected 1 prometheus pod but got %d", len(podList.Items))
		}

		// Load the remote write metrics
		expectedCadvisorMetrics, err := os.Open("./testdata/expected.cadvisor")
		if err != nil {
			t.Fatal(err)
		}

		defer expectedCadvisorMetrics.Close()

		// Compare the metrics
		_, err = execAndGetMetrics(ctx, cfg, podList.Items[0].Name, expectedCadvisorMetrics, []string{"container_cpu_usage_seconds_total", "container_memory_working_set_bytes"})
		if err != nil {
			t.Fatal(err)
		}

		return ctx
	}
}

func teardownPrometheus() features.Func {
	return func(ctx context.Context, _ *testing.T, _ *envconf.Config) context.Context {
		teardownKubeObjects([]k8s.Object{&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "prometheus"}}}, map[string]string{"app": "prometheus"})

		return ctx
	}
}

func teardownKubeObjects(objects []k8s.Object, labelSelectors map[string]string) features.Func {
	return func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
		r, err := resources.New(cfg.Client().RESTConfig())
		if err != nil {
			t.Fatal(err)
		}

		for _, object := range objects {
			object.SetNamespace(cfg.Namespace())

			if err = r.Delete(ctx, object); err != nil {
				t.Fatal(err)
			}
		}

		if labelSelectors != nil {
			err = wait.For(
				conditions.New(r).ResourceListN(
					&corev1.PodList{},
					0,
					resources.WithLabelSelector(labels.FormatLabels(labelSelectors)),
				),
				wait.WithInterval(200*time.Millisecond),
			)
			if err != nil {
				t.Fatal(err)
			}
		}

		return ctx
	}
}

// prometheusConfig reads in a base prometheus config and modifies the config
// to add in a scrape config for each cAdvisor instance as well as a remote write
// destination.
func prometheusConfig(numcAdvisors int) ([]byte, error) {

	// Read in our base config
	data, err := os.ReadFile("testdata/prometheus.yml")
	if err != nil {
		return nil, err
	}

	promConfig := config.DefaultConfig

	if err = yaml.Unmarshal(data, &promConfig); err != nil {
		return nil, err
	}

	// Configure the remote write endpoint
	u, err := url.Parse("http://localhost:8099/write")
	if err != nil {
		return nil, err
	}

	promConfig.RemoteWriteConfigs = []*config.RemoteWriteConfig{
		{
			URL: &common.URL{URL: u},
			HTTPClientConfig: common.HTTPClientConfig{
				EnableHTTP2: true,
			},
		},
	}

	// Add in a static scrape config for each cAdvisor instance
	var cadvisorScrapeConfig *config.ScrapeConfig

	for _, scrapeConfig := range promConfig.ScrapeConfigs {
		if scrapeConfig.JobName == "kubernetes-cadvisor" {
			cadvisorScrapeConfig = scrapeConfig
		}
	}

	target := &targetgroup.Group{Targets: []model.LabelSet{}}

	for i := 0; i < numcAdvisors; i++ {
		target.Targets = append(target.Targets, model.LabelSet{
			model.AddressLabel: model.LabelValue(fmt.Sprintf("localhost:81%02d", i)),
		})
	}

	staticConfig := discovery.StaticConfig{target}

	// Reset service discovery settings so we can explicitly set static targets
	cadvisorScrapeConfig.ServiceDiscoveryConfigs = discovery.Configs{staticConfig}

	// Clear out some existing settings
	cadvisorScrapeConfig.RelabelConfigs = nil
	cadvisorScrapeConfig.Scheme = "http"
	cadvisorScrapeConfig.HonorTimestamps = false

	promConfig.ScrapeConfigs = []*config.ScrapeConfig{cadvisorScrapeConfig}

	return yaml.Marshal(promConfig)
}

func generateUpDog(numcAdvisors int) string {
	var sb strings.Builder

	for i := 0; i < numcAdvisors; i++ {
		sb.WriteString(fmt.Sprintf("up{instance=\"localhost:81%02d\",job=\"kubernetes-cadvisor\"} 1\n", i))
	}

	return sb.String()
}

func execAndGetMetrics(ctx context.Context, cfg *envconf.Config, podName string, expected io.Reader, metricNames []string) (bool, error) {
	var stdout, stderr bytes.Buffer

	if err := cfg.Client().Resources().ExecInPod(
		ctx,
		cfg.Namespace(),
		podName,
		"remote-write",
		[]string{"curl", "-s", "http://localhost:8099/metrics"},
		&stdout, &stderr,
	); err != nil {
		return false, err
	}

	if stderr.String() != "" {
		return false, fmt.Errorf("stderr: %s", stderr.String())
	}

	// This is overkill, but fits the bill
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintln(w, stdout.String())
	}))
	defer ts.Close()

	err := testutil.ScrapeAndCompare(ts.URL, expected, metricNames...)
	if err != nil {
		return false, err
	}

	return true, nil
}

func newPrometheusDeployment(numcAdvisors int) *appsv1.Deployment {
	deployment := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name: "prometheus",
		},
		Spec: appsv1.DeploymentSpec{
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{
					"app": "prometheus",
				},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"app": "prometheus",
					},
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:  "prometheus",
							Image: "prom/prometheus:v2.44.0",
							VolumeMounts: []corev1.VolumeMount{
								{
									Name:      "prometheus-config",
									MountPath: "/etc/prometheus",
								},
							},
						},
						{
							Name:  "remote-write",
							Image: "golang:latest",
							Command: []string{
								"/bin/bash",
							},
							Args: []string{
								"-c",
								// Need to do copy here so we can have a writable directory
								"cp -r /go/remote-write /go/remote-write-copy && cd /go/remote-write-copy && go mod tidy && go run . --port=8099",
							},
							VolumeMounts: []corev1.VolumeMount{
								{
									Name:      "remote-write-go",
									MountPath: "/go/remote-write",
								},
							},
						},
					},
					Volumes: []corev1.Volume{
						{
							Name: "prometheus-config",
							VolumeSource: corev1.VolumeSource{
								ConfigMap: &corev1.ConfigMapVolumeSource{
									LocalObjectReference: corev1.LocalObjectReference{
										Name: "prometheus-config",
									},
								},
							},
						},
						{
							Name: "cadvisor-go",
							VolumeSource: corev1.VolumeSource{
								ConfigMap: &corev1.ConfigMapVolumeSource{
									LocalObjectReference: corev1.LocalObjectReference{
										Name: "cadvisor-go",
									},
								},
							},
						},
						{
							Name: "remote-write-go",
							VolumeSource: corev1.VolumeSource{
								ConfigMap: &corev1.ConfigMapVolumeSource{
									LocalObjectReference: corev1.LocalObjectReference{
										Name: "remote-write-go",
									},
								},
							},
						},
					},
				},
			},
		},
	}

	for i := 0; i < numcAdvisors; i++ {
		deployment.Spec.Template.Spec.Containers = append(deployment.Spec.Template.Spec.Containers, corev1.Container{
			Name:  fmt.Sprintf("cadvisor-%d", i),
			Image: "golang:latest",
			Command: []string{
				"/bin/bash",
			},
			Args: []string{
				"-c",
				"cd /go/cadvisor && go run . --cadvisorFile=/etc/cadvisor/cadvisor.prom --port=81" + fmt.Sprintf("%02d", i),
			},
			VolumeMounts: []corev1.VolumeMount{
				{
					Name:      fmt.Sprintf("cadvisor-%d", i),
					MountPath: "/etc/cadvisor",
				},
				{
					Name:      "cadvisor-go",
					MountPath: "/go/cadvisor",
				},
			},
		})

		deployment.Spec.Template.Spec.Volumes = append(deployment.Spec.Template.Spec.Volumes, corev1.Volume{
			Name: fmt.Sprintf("cadvisor-%d", i),
			VolumeSource: corev1.VolumeSource{
				ConfigMap: &corev1.ConfigMapVolumeSource{
					LocalObjectReference: corev1.LocalObjectReference{
						Name: fmt.Sprintf("cadvisor-%d", i),
					},
				},
			},
		})
	}

	return deployment
}

func gzipCadvisorConfigmaps() ([]*corev1.ConfigMap, error) {
	configmaps := []*corev1.ConfigMap{}

	cadvisorIdx := 0

	err := filepath.Walk("./testdata", func(path string, info fs.FileInfo, err error) error {
		if err != nil {
			return err
		}

		if info.IsDir() {
			return nil
		}

		if !strings.HasSuffix(info.Name(), ".cadvisor") {
			return nil
		}

		f, err := os.Open(path)
		if err != nil {
			return err
		}

		defer f.Close()

		var encodedData bytes.Buffer

		b64encoder := base64.NewEncoder(base64.StdEncoding, &encodedData)
		gzipWriter := gzip.NewWriter(b64encoder)

		if _, err = io.Copy(gzipWriter, f); err != nil && err != io.ErrClosedPipe {
			return err
		}

		if err = gzipWriter.Close(); err != nil && err != io.ErrClosedPipe {
			return err
		}

		if err = b64encoder.Close(); err != nil && err != io.ErrClosedPipe {
			return err
		}

		// Now we have a base64 encoded gzip blob
		// so we can create our configmap
		configmap := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name: fmt.Sprintf("cadvisor-%d", cadvisorIdx),
			},
			Data: map[string]string{
				"cadvisor.prom": encodedData.String(),
			},
		}

		configmaps = append(configmaps, configmap)

		cadvisorIdx++

		return nil
	})
	if err != nil {
		return nil, err
	}

	return configmaps, nil
}

func setupcAdvisors() ([]*corev1.ConfigMap, *corev1.ConfigMap, error) {
	cadvisorConfigs, err := gzipCadvisorConfigmaps()
	if err != nil {
		return nil, nil, err
	}

	cadvisorGoMod, err := os.ReadFile("./cmd/fake-cadvisor/go.mod")
	if err != nil {
		return nil, nil, err
	}

	cadvisorGoMain, err := os.ReadFile("./cmd/fake-cadvisor/main.go")
	if err != nil {
		return nil, nil, err
	}

	cadvisorProgram := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "cadvisor-go"},
		Data: map[string]string{
			"go.mod":  string(cadvisorGoMod),
			"main.go": string(cadvisorGoMain),
		},
	}

	return cadvisorConfigs, cadvisorProgram, nil
}

func setupRemoteWrite() (*corev1.ConfigMap, error) {
	remoteWriteGoMod, err := os.ReadFile("./cmd/remote-write/go.mod")
	if err != nil {
		return nil, err
	}

	remoteWriteGoMain, err := os.ReadFile("./cmd/remote-write/main.go")
	if err != nil {
		return nil, err
	}

	remoteWriteProgram := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "remote-write-go"},
		Data: map[string]string{
			"go.mod":  string(remoteWriteGoMod),
			"main.go": string(remoteWriteGoMain),
		},
	}

	return remoteWriteProgram, nil
}

func setupPrometheus(numcAdvisors int) (*appsv1.Deployment, *corev1.ConfigMap, error) {
	promConfigData, err := prometheusConfig(numcAdvisors)
	if err != nil {
		return nil, nil, err
	}

	promConfigMap := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "prometheus-config"},
		Data: map[string]string{
			"prometheus.yml": string(promConfigData),
		},
	}

	promDeployment := newPrometheusDeployment(numcAdvisors)

	return promDeployment, promConfigMap, nil
}
