package k8sclient

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-k8s-tester/pkg/fileutil"
	"github.com/aws/aws-k8s-tester/pkg/httputil"
	"github.com/aws/aws-k8s-tester/pkg/logutil"
	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/sts"
	"go.uber.org/zap"
	"golang.org/x/oauth2"
	apps_v1 "k8s.io/api/apps/v1"
	apps_v1beta1 "k8s.io/api/apps/v1beta1"
	apps_v1beta2 "k8s.io/api/apps/v1beta2"
	v1 "k8s.io/api/core/v1"
	extensions_v1beta1 "k8s.io/api/extensions/v1beta1"
	networking_v1 "k8s.io/api/networking/v1"
	policy_v1beta1 "k8s.io/api/policy/v1beta1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/version"
	"k8s.io/client-go/kubernetes"
	restclient "k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	"k8s.io/utils/exec"
	"sigs.k8s.io/yaml"
)

// EKSConfig defines EKS client configuration.
type EKSConfig struct {
	// Logger is the logger to log client operations.
	Logger *zap.Logger
	// Region is used for EKS auth provider configuration.
	Region string
	// ClusterName is the EKS cluster name.
	// Used for EKS auth provider configuration.
	ClusterName string
	// ClusterAPIServerEndpoint is the EKS kube-apiserver endpoint.
	// Use for kubeconfig.
	ClusterAPIServerEndpoint string
	// ClusterCADecoded is the cluster CA base64-decoded.
	// Use for kubeconfig.
	ClusterCADecoded string
	// KubectlPath is the kubectl path, used for health checks.
	KubectlPath string
	// KubeConfigPath is the kubeconfig path to load.
	KubeConfigPath string
	// KubeConfigContext is the kubeconfig context.
	KubeConfigContext string
	// ServerVersion is the kube-apiserver version.
	// If not empty, this is used for health checks.
	ServerVersion string
	// EncryptionEnabled is true if EKS cluster is created with KMS encryption enabled.
	// If true, the health check checks if data encryption key has been generated
	// to encrypt initial service account tokens, via kube-apiserver metrics endpoint.
	EncryptionEnabled bool
	// EnablePrompt is true to enable interactive mode.
	EnablePrompt bool
	// Dir is the directory to store all upgrade/rollback files.
	Dir string

	// Clients is the number of kubernetes clients to create.
	// Default is 1.
	Clients int
	// ClientQPS is the QPS for kubernetes client.
	// To use while talking with kubernetes apiserver.
	//
	// Kubernetes client DefaultQPS is 5.
	// Kubernetes client DefaultBurst is 10.
	// ref. https://github.com/kubernetes/kubernetes/blob/4d0e86f0b8d1eae00a202009858c8739e4c9402e/staging/src/k8s.io/client-go/rest/config.go#L43-L46
	//
	// kube-apiserver default inflight requests limits are:
	// FLAG: --max-mutating-requests-inflight="200"
	// FLAG: --max-requests-inflight="400"
	// ref. https://github.com/kubernetes/kubernetes/blob/4d0e86f0b8d1eae00a202009858c8739e4c9402e/staging/src/k8s.io/apiserver/pkg/server/config.go#L300-L301
	//
	ClientQPS float32
	// ClientBurst is the burst for kubernetes client.
	// To use while talking with kubernetes apiserver
	//
	// Kubernetes client DefaultQPS is 5.
	// Kubernetes client DefaultBurst is 10.
	// ref. https://github.com/kubernetes/kubernetes/blob/4d0e86f0b8d1eae00a202009858c8739e4c9402e/staging/src/k8s.io/client-go/rest/config.go#L43-L46
	//
	// kube-apiserver default inflight requests limits are:
	// FLAG: --max-mutating-requests-inflight="200"
	// FLAG: --max-requests-inflight="400"
	// ref. https://github.com/kubernetes/kubernetes/blob/4d0e86f0b8d1eae00a202009858c8739e4c9402e/staging/src/k8s.io/apiserver/pkg/server/config.go#L300-L301
	//
	ClientBurst int
	// ClientTimeout is the client timeout.
	ClientTimeout time.Duration
}

// EKS defines EKS client operations.
type EKS interface {
	// KubernetesClientSet returns a new kubernetes client set.
	KubernetesClientSet() *kubernetes.Clientset

	// CheckEKSHealth checks the EKS health.
	CheckHealth() error

	// FetchServerVersion fetches the version from kube-apiserver.
	//
	// e.g.
	//
	//	{
	//		"major": "1",
	//		"minor": "16+",
	//		"gitVersion": "v1.16.8-eks-e16311",
	//		"gitCommit": "e163110a04dcb2f39c3325af96d019b4925419eb",
	//		"gitTreeState": "clean",
	//		"buildDate": "2020-03-27T22:37:12Z",
	//		"goVersion": "go1.13.8",
	//		"compiler": "gc",
	//		"platform": "linux/amd64"
	//	}
	//
	FetchServerVersion() (ServerVersionInfo, error)

	// FetchSupportedAPIGroupVersions fetches all supported API group resources.
	// ref. https://github.com/kubernetes/kubernetes/tree/master/staging/src/k8s.io/kubectl/pkg/cmd/apiresources
	FetchSupportedAPIGroupVersions() (float64, map[string]struct{}, error)

	// ListNamespaces returns the list of existing namespace names.
	ListNamespaces(batchLimit int64, batchInterval time.Duration) ([]v1.Namespace, error)
	// ListNodes returns the list of existing nodes.
	ListNodes(batchLimit int64, batchInterval time.Duration) ([]v1.Node, error)
	// ListPods returns the list of existing namespace names.
	ListPods(namespace string, batchLimit int64, batchInterval time.Duration) ([]v1.Pod, error)
	// ListSecrets returns the list of existing Secret objects.
	ListSecrets(namespace string, batchLimit int64, batchInterval time.Duration) ([]v1.Secret, error)

	ListAppsV1Deployments(namespace string, batchLimit int64, batchInterval time.Duration) (ss []apps_v1.Deployment, err error)
	ListAppsV1StatefulSets(namespace string, batchLimit int64, batchInterval time.Duration) (ss []apps_v1.StatefulSet, err error)
	ListAppsV1DaemonSets(namespace string, batchLimit int64, batchInterval time.Duration) (ss []apps_v1.DaemonSet, err error)
	ListAppsV1ReplicaSets(namespace string, batchLimit int64, batchInterval time.Duration) (ss []apps_v1.ReplicaSet, err error)
	ListNetworkingV1NetworkPolicies(namespace string, batchLimit int64, batchInterval time.Duration) (ss []networking_v1.NetworkPolicy, err error)
	ListPolicyV1beta1PodSecurityPolicies(batchLimit int64, batchInterval time.Duration) (ss []policy_v1beta1.PodSecurityPolicy, err error)

	ListAppsV1beta1Deployments(namespace string, batchLimit int64, batchInterval time.Duration) (ss []apps_v1beta1.Deployment, err error)
	ListAppsV1beta1StatefulSets(namespace string, batchLimit int64, batchInterval time.Duration) (ss []apps_v1beta1.StatefulSet, err error)
	ListAppsV1beta2Deployments(namespace string, batchLimit int64, batchInterval time.Duration) (ss []apps_v1beta2.Deployment, err error)
	ListAppsV1beta2StatefulSets(namespace string, batchLimit int64, batchInterval time.Duration) (ss []apps_v1beta2.StatefulSet, err error)
	ListExtensionsV1beta1DaemonSets(namespace string, batchLimit int64, batchInterval time.Duration) (ss []extensions_v1beta1.DaemonSet, err error)
	ListExtensionsV1beta1Deployments(namespace string, batchLimit int64, batchInterval time.Duration) (ss []extensions_v1beta1.Deployment, err error)
	ListExtensionsV1beta1ReplicaSets(namespace string, batchLimit int64, batchInterval time.Duration) (ss []extensions_v1beta1.ReplicaSet, err error)
	ListExtensionsV1beta1NetworkPolicies(namespace string, batchLimit int64, batchInterval time.Duration) (ss []extensions_v1beta1.NetworkPolicy, err error)
	ListExtensionsV1beta1PodSecurityPolicies(batchLimit int64, batchInterval time.Duration) (ss []extensions_v1beta1.PodSecurityPolicy, err error)

	// GetObject get object type and object metadata using kubectl.
	// The internal API group version is not exposed,
	// thus kubectl converts API version internally.
	// ref. https://github.com/kubernetes/kubernetes/issues/58131#issuecomment-403829566
	GetObject(namespace string, kind string, name string) (obj Object, d []byte, err error)

	// Deprecate checks deprecated API groups based on the current kube-apiserver version.
	Deprecate(batchLimit int64, batchInterval time.Duration) error
}

type eks struct {
	cfg *EKSConfig

	mu      sync.Mutex
	clients []*kubernetes.Clientset
	cur     int
}

// NewEKS returns a new EKS client.
func NewEKS(cfg *EKSConfig) (e EKS, err error) {
	if cfg == nil {
		return nil, errors.New("nil EKSConfig")
	}
	if cfg.Logger == nil {
		var err error
		cfg.Logger, err = logutil.GetDefaultZapLogger()
		if err != nil {
			return nil, err
		}
	}

	if cfg.Dir == "" {
		cfg.Dir, err = ioutil.TempDir(os.TempDir(), "eks-dir")
		if err != nil {
			return nil, err
		}
	}
	if err = os.MkdirAll(cfg.Dir, 0700); err != nil {
		return nil, err
	}
	cfg.Logger.Info("created dir", zap.String("dir", cfg.Dir))

	if cfg.Clients < 1 {
		cfg.Clients = 1
	}

	cfg.Logger.Info("creating clients", zap.String("kubeconfig", cfg.KubeConfigPath))
	ek := &eks{cfg: cfg, clients: make([]*kubernetes.Clientset, cfg.Clients)}
	for i := 0; i < cfg.Clients; i++ {
		ek.clients[i], err = createClient(cfg)
		if err != nil {
			cfg.Logger.Warn("failed to create client", zap.Int("index", i), zap.Error(err))
			return nil, err
		}
		cfg.Logger.Info("added a client", zap.Int("index", i), zap.Int("total-clients", cfg.Clients))
	}
	e = ek
	return ek, nil
}

func createClient(cfg *EKSConfig) (cli *kubernetes.Clientset, err error) {
	var kcfg *restclient.Config
	if cfg.KubeConfigPath != "" {
		switch {
		case cfg.KubeConfigContext != "":
			cfg.Logger.Info("creating k8s client using KUBECONFIG and context",
				zap.String("kubeconfig", cfg.KubeConfigPath),
				zap.String("context", cfg.KubeConfigContext),
			)
			kcfg, err = clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
				&clientcmd.ClientConfigLoadingRules{
					ExplicitPath: cfg.KubeConfigPath,
				},
				&clientcmd.ConfigOverrides{
					CurrentContext: cfg.KubeConfigContext,
					ClusterInfo:    clientcmdapi.Cluster{Server: ""},
				},
			).ClientConfig()

		case cfg.KubeConfigContext == "":
			cfg.Logger.Info("creating k8s client using KUBECONFIG",
				zap.String("kubeconfig", cfg.KubeConfigPath),
			)
			kcfg, err = clientcmd.BuildConfigFromFlags("", cfg.KubeConfigPath)
		}

		if err != nil {
			cfg.Logger.Warn("failed to read kubeconfig", zap.Error(err))
		}
	}
	if kcfg == nil {
		kcfg = createClientConfigEKS(cfg)
	}
	if kcfg == nil {
		defaultConfig := clientcmd.DefaultClientConfig
		kcfg, _ = defaultConfig.ClientConfig()
	}
	if kcfg == nil {
		cfg.Logger.Warn("failed to create k8s client config")
		return nil, errors.New("failed to create k8s client config")
	}

	cfg.Logger.Info("loaded k8s client config",
		zap.String("host", kcfg.Host),
		zap.String("user-name", kcfg.Username),
		zap.String("server-name", kcfg.ServerName),
	)

	if cfg.ClusterAPIServerEndpoint == "" {
		cfg.ClusterAPIServerEndpoint = kcfg.Host
		cfg.Logger.Info("updated apiserver endpoint from kubeconfig", zap.String("apiserver-endpoint", kcfg.Host))
	} else if cfg.ClusterAPIServerEndpoint != kcfg.Host {
		cfg.Logger.Warn("unexpected apiserver endpoint",
			zap.String("apiserver-endpoint", cfg.ClusterAPIServerEndpoint),
			zap.String("kubeconfig-host", kcfg.Host),
		)
	}
	if cfg.ClusterAPIServerEndpoint == "" {
		return nil, errors.New("empty ClusterAPIServerEndpoint")
	}

	if cfg.ClusterCADecoded == "" {
		cfg.ClusterCADecoded = string(kcfg.TLSClientConfig.CAData)
		cfg.Logger.Info("updated cluster ca from kubeconfig", zap.String("cluster-ca", string(kcfg.TLSClientConfig.CAData)))
	} else if cfg.ClusterCADecoded != string(kcfg.TLSClientConfig.CAData) {
		cfg.Logger.Warn("unexpected cluster ca",
			zap.String("cluster-ca", cfg.ClusterCADecoded),
			zap.String("kubeconfig-cluster-ca", string(kcfg.TLSClientConfig.CAData)),
		)
	}
	if cfg.ClusterCADecoded == "" {
		cfg.Logger.Warn("no cluster CA found in k8s client")
	}

	if kcfg.AuthProvider != nil {
		if cfg.Region == "" {
			cfg.Region = kcfg.AuthProvider.Config["region"]
			cfg.Logger.Info("updated region from kubeconfig", zap.String("apiserver-endpoint", kcfg.AuthProvider.Config["region"]))
		} else if cfg.Region != kcfg.AuthProvider.Config["region"] {
			cfg.Logger.Warn("unexpected region",
				zap.String("apiserver-endpoint", cfg.Region),
				zap.String("kubeconfig-host", kcfg.AuthProvider.Config["region"]),
			)
		}
		if cfg.ClusterName == "" {
			cfg.ClusterName = kcfg.AuthProvider.Config["cluster-name"]
			cfg.Logger.Info("updated cluster-name from kubeconfig", zap.String("apiserver-endpoint", kcfg.AuthProvider.Config["cluster-name"]))
		} else if cfg.ClusterName != kcfg.AuthProvider.Config["cluster-name"] {
			cfg.Logger.Warn("unexpected cluster-name",
				zap.String("apiserver-endpoint", cfg.ClusterName),
				zap.String("kubeconfig-host", kcfg.AuthProvider.Config["cluster-name"]),
			)
		}
	}
	if cfg.Region == "" {
		cfg.Logger.Warn("no region found in k8s client")
	}
	if cfg.ClusterName == "" {
		cfg.Logger.Warn("no cluster name found in k8s client")
	}

	if cfg.ClientQPS > 0 {
		kcfg.QPS = cfg.ClientQPS
	}
	if cfg.ClientBurst > 0 {
		kcfg.Burst = cfg.ClientBurst
	}
	if cfg.ClientTimeout > 0 {
		kcfg.Timeout = cfg.ClientTimeout
	}

	cli, err = kubernetes.NewForConfig(kcfg)
	if err != nil {
		cfg.Logger.Warn("failed to create k8s client", zap.Error(err))
		return nil, err
	}
	cfg.Logger.Info("created k8s client", zap.Float32("qps", kcfg.QPS), zap.Int("burst", kcfg.Burst))
	return cli, nil
}

// ServerVersionInfo is the server version info from kube-apiserver
type ServerVersionInfo struct {
	version.Info
	VersionValue float64 `json:"version-value"`
}

func (sv ServerVersionInfo) String() string {
	d, err := json.MarshalIndent(sv, "", "    ")
	if err != nil {
		return sv.GitVersion
	}
	return string(d)
}

func (e *eks) getClient() *kubernetes.Clientset {
	e.mu.Lock()
	if len(e.clients) == 0 {
		e.mu.Unlock()
		return nil
	}
	e.cur = (e.cur + 1) % len(e.clients)
	cli := e.clients[e.cur]
	e.mu.Unlock()
	return cli
}

// KubernetesClientSet returns a new kubernetes client set.
func (e *eks) KubernetesClientSet() *kubernetes.Clientset {
	return e.getClient()
}

const authProviderName = "eks"

func createClientConfigEKS(cfg *EKSConfig) *restclient.Config {
	if cfg.Region == "" {
		return nil
	}
	if cfg.ClusterName == "" {
		return nil
	}
	if cfg.ClusterAPIServerEndpoint == "" {
		return nil
	}
	if cfg.ClusterCADecoded == "" {
		return nil
	}
	cfg.Logger.Info("creating k8s client using status")
	return &restclient.Config{
		Host: cfg.ClusterAPIServerEndpoint,
		TLSClientConfig: restclient.TLSClientConfig{
			CAData: []byte(cfg.ClusterCADecoded),
		},
		AuthProvider: &clientcmdapi.AuthProviderConfig{
			Name: authProviderName,
			Config: map[string]string{
				"region":       cfg.Region,
				"cluster-name": cfg.ClusterName,
			},
		},
	}
}

func init() {
	restclient.RegisterAuthProviderPlugin(authProviderName, newAuthProviderEKS)
}

func newAuthProviderEKS(_ string, config map[string]string, _ restclient.AuthProviderConfigPersister) (restclient.AuthProvider, error) {
	awsRegion, ok := config["region"]
	if !ok {
		return nil, fmt.Errorf("'clientcmdapi.AuthProviderConfig' does not include 'region' key %+v", config)
	}
	clusterName, ok := config["cluster-name"]
	if !ok {
		return nil, fmt.Errorf("'clientcmdapi.AuthProviderConfig' does not include 'cluster-name' key %+v", config)
	}

	sess := session.Must(session.NewSession(aws.NewConfig().WithRegion(awsRegion)))
	return &eksAuthProvider{ts: newTokenSourceEKS(sess, clusterName)}, nil
}

type eksAuthProvider struct {
	ts oauth2.TokenSource
}

func (p *eksAuthProvider) WrapTransport(rt http.RoundTripper) http.RoundTripper {
	return &oauth2.Transport{
		Source: p.ts,
		Base:   rt,
	}
}

func (p *eksAuthProvider) Login() error {
	return nil
}

func newTokenSourceEKS(sess *session.Session, clusterName string) oauth2.TokenSource {
	return &eksTokenSource{sess: sess, clusterName: clusterName}
}

type eksTokenSource struct {
	sess        *session.Session
	clusterName string
}

// Reference
// https://github.com/kubernetes-sigs/aws-iam-authenticator/blob/master/README.md#api-authorization-from-outside-a-cluster
// https://github.com/kubernetes-sigs/aws-iam-authenticator/blob/master/pkg/token/token.go
const (
	v1Prefix        = "k8s-aws-v1."
	clusterIDHeader = "x-k8s-aws-id"
)

func (s *eksTokenSource) Token() (*oauth2.Token, error) {
	stsAPI := sts.New(s.sess)
	request, _ := stsAPI.GetCallerIdentityRequest(&sts.GetCallerIdentityInput{})
	request.HTTPRequest.Header.Add(clusterIDHeader, s.clusterName)

	payload, err := request.Presign(60)
	if err != nil {
		return nil, err
	}
	token := v1Prefix + base64.RawURLEncoding.EncodeToString([]byte(payload))
	tokenExpiration := time.Now().Local().Add(14 * time.Minute)
	return &oauth2.Token{
		AccessToken: token,
		TokenType:   "Bearer",
		Expiry:      tokenExpiration,
	}, nil
}

// CheckHealth checks the EKS health.
func (e *eks) CheckHealth() error {
	err := e.checkHealth()
	return err
}

func (e *eks) checkHealth() error {
	if e.cfg == nil {
		return errors.New("nil EKSConfig")
	}
	if e.cfg.KubectlPath == "" {
		return errors.New("empty EKSConfig.KubectlPath")
	}
	if e.cfg.KubeConfigPath == "" {
		return errors.New("empty EKSConfig.KubeConfigPath")
	}
	if e.cfg.ClusterAPIServerEndpoint == "" {
		return errors.New("empty EKSConfig.ClusterAPIServerEndpoint")
	}

	if !fileutil.Exist(e.cfg.KubeConfigPath) {
		return fmt.Errorf("%q not found", e.cfg.KubeConfigPath)
	}
	if !fileutil.Exist(e.cfg.KubectlPath) {
		return fmt.Errorf("%q not found", e.cfg.KubectlPath)
	}
	if err := fileutil.EnsureExecutable(e.cfg.KubectlPath); err != nil {
		return fmt.Errorf("cannot execute %q (%v)", e.cfg.KubectlPath, err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	output, err := exec.New().CommandContext(
		ctx,
		e.cfg.KubectlPath,
		"--kubeconfig="+e.cfg.KubeConfigPath,
		"version",
	).CombinedOutput()
	cancel()
	out := strings.TrimSpace(string(output))
	if err != nil {
		return fmt.Errorf("'kubectl version' failed %v (output %q)", err, out)
	}
	fmt.Printf("\n\"kubectl version\" output:\n%s\n\n", out)

	vf, err := e.fetchServerVersion()
	if err != nil {
		return fmt.Errorf("fetch version info failed %v", err)
	}
	fmt.Printf("\n\"kubectl version\" info output:\n%s\n\n", vf.String())

	ep := e.cfg.ClusterAPIServerEndpoint + "/version"
	output, err = httputil.ReadInsecure(e.cfg.Logger, os.Stderr, ep)
	if err != nil {
		return err
	}
	out = strings.TrimSpace(string(output))
	if e.cfg.ServerVersion != "" && !strings.Contains(out, fmt.Sprintf(`"gitVersion": "v%s`, e.cfg.ServerVersion)) {
		return fmt.Errorf("%q does not contain version %q", out, e.cfg.ServerVersion)
	}
	fmt.Printf("\n\n\"%s\" output:\n%s\n\n", ep, out)

	ctx, cancel = context.WithTimeout(context.Background(), 15*time.Second)
	output, err = exec.New().CommandContext(
		ctx,
		e.cfg.KubectlPath,
		"--kubeconfig="+e.cfg.KubeConfigPath,
		"cluster-info",
	).CombinedOutput()
	cancel()
	out = strings.TrimSpace(string(output))
	if err != nil {
		return fmt.Errorf("'kubectl cluster-info' failed %v (output %q)", err, out)
	}
	if !strings.Contains(out, "is running at") {
		return fmt.Errorf("'kubectl cluster-info' not ready (output %q)", out)
	}
	fmt.Printf("\n\"kubectl cluster-info\" output:\n%s\n\n", out)

	ctx, cancel = context.WithTimeout(context.Background(), 15*time.Second)
	output, err = exec.New().CommandContext(
		ctx,
		e.cfg.KubectlPath,
		"--kubeconfig="+e.cfg.KubeConfigPath,
		"get",
		"cs",
	).CombinedOutput()
	cancel()
	out = strings.TrimSpace(string(output))
	if err != nil {
		return fmt.Errorf("'kubectl get cs' failed %v (output %q)", err, out)
	}
	fmt.Printf("\n\"kubectl get cs\" output:\n%s\n\n", out)

	ep = e.cfg.ClusterAPIServerEndpoint + "/healthz?verbose"
	output, err = httputil.ReadInsecure(e.cfg.Logger, os.Stderr, ep)
	if err != nil {
		return err
	}
	out = strings.TrimSpace(string(output))
	if !strings.Contains(out, "healthz check passed") {
		return fmt.Errorf("%q does not contain 'healthz check passed'", out)
	}
	fmt.Printf("\n\n\"%s\" output (\"kubectl get --raw /healthz?verbose\"):\n%s\n", ep, out)

	ctx, cancel = context.WithTimeout(context.Background(), 15*time.Second)
	output, err = exec.New().CommandContext(
		ctx,
		e.cfg.KubectlPath,
		"--kubeconfig="+e.cfg.KubeConfigPath,
		"--namespace=kube-system",
		"get",
		"all",
	).CombinedOutput()
	cancel()
	out = strings.TrimSpace(string(output))
	if err != nil {
		return fmt.Errorf("'kubectl get all -n=kube-system' failed %v (output %q)", err, out)
	}
	fmt.Printf("\n\"kubectl all -n=kube-system\" output:\n%s\n\n", out)

	fmt.Printf("\n\"kubectl get pods -n=kube-system\" output:\n")
	ctx, cancel = context.WithTimeout(context.Background(), time.Minute)
	pods, err := e.getClient().CoreV1().Pods("kube-system").List(ctx, metav1.ListOptions{})
	cancel()
	if err != nil {
		return fmt.Errorf("failed to get pods %v", err)
	}
	for _, v := range pods.Items {
		fmt.Printf("kube-system Pod: %q\n", v.Name)
	}
	println()

	ctx, cancel = context.WithTimeout(context.Background(), 15*time.Second)
	output, err = exec.New().CommandContext(
		ctx,
		e.cfg.KubectlPath,
		"--kubeconfig="+e.cfg.KubeConfigPath,
		"--namespace=kube-system",
		"get",
		"configmaps",
	).CombinedOutput()
	cancel()
	out = strings.TrimSpace(string(output))
	if err != nil {
		return fmt.Errorf("'kubectl get configmaps --namespace=kube-system' failed %v (output %q)", err, out)
	}
	fmt.Printf("\n\"kubectl get configmaps --namespace=kube-system\" output:\n%s\n\n", out)

	ctx, cancel = context.WithTimeout(context.Background(), 15*time.Second)
	output, err = exec.New().CommandContext(
		ctx,
		e.cfg.KubectlPath,
		"--kubeconfig="+e.cfg.KubeConfigPath,
		"get",
		"namespaces",
	).CombinedOutput()
	cancel()
	out = strings.TrimSpace(string(output))
	if err != nil {
		return fmt.Errorf("'kubectl get namespaces' failed %v (output %q)", err, out)
	}
	fmt.Printf("\n\"kubectl get namespaces\" output:\n%s\n\n", out)

	ctx, cancel = context.WithTimeout(context.Background(), 15*time.Second)
	output, err = exec.New().CommandContext(
		ctx,
		e.cfg.KubectlPath,
		"--kubeconfig="+e.cfg.KubeConfigPath,
		"get",
		"nodes",
		"--show-labels",
		"-o=wide",
	).CombinedOutput()
	cancel()
	out = strings.TrimSpace(string(output))
	if err != nil {
		return fmt.Errorf("'kubectl get nodes --show-labels -o=wide' failed %v (output %q)", err, out)
	}
	fmt.Printf("\n\"kubectl get nodes --show-labels -o=wide\" output:\n%s\n\n", out)

	fmt.Printf("\n\"curl -sL http://localhost:8080/metrics | grep storage_\" output:\n")
	ctx, cancel = context.WithTimeout(context.Background(), time.Minute)
	output, err = e.getClient().
		CoreV1().
		RESTClient().
		Get().
		RequestURI("/metrics").
		Do(ctx).
		Raw()
	cancel()
	if err != nil {
		return fmt.Errorf("failed to fetch /metrics (%v)", err)
	}
	const (
		// https://github.com/kubernetes/kubernetes/blob/master/CHANGELOG/CHANGELOG-1.17.md#deprecatedchanged-metrics
		metricDEKGenSecondsCount      = "apiserver_storage_data_key_generation_duration_seconds_count"
		metricDEKGenMicroSecondsCount = "apiserver_storage_data_key_generation_latencies_microseconds_count"
		metricEnvelopeCacheMiss       = "apiserver_storage_envelope_transformation_cache_misses_total"
	)
	dekGenCnt, cacheMissCnt := int64(0), int64(0)
	scanner := bufio.NewScanner(bytes.NewReader(output))
	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case strings.HasPrefix(line, "# "):
			continue

		// https://github.com/kubernetes/kubernetes/blob/master/CHANGELOG/CHANGELOG-1.17.md#deprecatedchanged-metrics
		case strings.HasPrefix(line, metricDEKGenSecondsCount+" "):
			vs := strings.TrimSpace(strings.Replace(line, metricDEKGenSecondsCount, "", -1))
			dekGenCnt, err = strconv.ParseInt(vs, 10, 64)
			if err != nil {
				e.cfg.Logger.Warn("failed to parse",
					zap.String("line", line),
					zap.Error(err),
				)
			}

		// https://github.com/kubernetes/kubernetes/blob/master/CHANGELOG/CHANGELOG-1.17.md#deprecatedchanged-metrics
		case strings.HasPrefix(line, metricDEKGenMicroSecondsCount+" "):
			vs := strings.TrimSpace(strings.Replace(line, metricDEKGenMicroSecondsCount, "", -1))
			dekGenCnt, err = strconv.ParseInt(vs, 10, 64)
			if err != nil {
				e.cfg.Logger.Warn("failed to parse",
					zap.String("line", line),
					zap.Error(err),
				)
			}

		case strings.HasPrefix(line, metricEnvelopeCacheMiss+" "):
			vs := strings.TrimSpace(strings.Replace(line, metricEnvelopeCacheMiss, "", -1))
			cacheMissCnt, err = strconv.ParseInt(vs, 10, 64)
			if err != nil {
				e.cfg.Logger.Warn("failed to parse",
					zap.String("line", line),
					zap.Error(err),
				)
			}
		}

		if dekGenCnt > 0 || cacheMissCnt > 0 {
			break
		}
	}
	e.cfg.Logger.Info("encryption metrics",
		zap.Int64("dek-gen-count", dekGenCnt),
		zap.Int64("cache-miss-count", cacheMissCnt),
	)
	if e.cfg.EncryptionEnabled {
		if dekGenCnt == 0 && cacheMissCnt == 0 {
			return errors.New("encrypted enabled, unexpected /metrics")
		}
		e.cfg.Logger.Info("successfully checked encryption")
	}

	e.cfg.Logger.Info("checked /metrics")
	return nil
}

// FetchServerVersion fetches the version from kube-apiserver.
//
// e.g.
//
//	{
//		"major": "1",
//		"minor": "16+",
//		"gitVersion": "v1.16.8-eks-e16311",
//		"gitCommit": "e163110a04dcb2f39c3325af96d019b4925419eb",
//		"gitTreeState": "clean",
//		"buildDate": "2020-03-27T22:37:12Z",
//		"goVersion": "go1.13.8",
//		"compiler": "gc",
//		"platform": "linux/amd64"
//	}
//
func (e *eks) FetchServerVersion() (ServerVersionInfo, error) {
	ver, err := e.fetchServerVersion()
	return ver, err
}

func (e *eks) fetchServerVersion() (ServerVersionInfo, error) {
	ep := e.cfg.ClusterAPIServerEndpoint + "/version"
	e.cfg.Logger.Info("fetching version", zap.String("url", ep))
	d, err := httputil.ReadInsecure(e.cfg.Logger, os.Stderr, ep)
	if err != nil {
		return ServerVersionInfo{}, nil
	}
	return parseVersion(e.cfg.Logger, d)
}

var regex = regexp.MustCompile("[^0-9]+")

func parseVersion(lg *zap.Logger, d []byte) (ServerVersionInfo, error) {
	var ver ServerVersionInfo
	err := json.NewDecoder(bytes.NewReader(d)).Decode(&ver)
	if err != nil {
		lg.Warn("failed to fetch version", zap.Error(err))
		return ServerVersionInfo{}, err
	}
	ver.VersionValue, _ = strconv.ParseFloat(ver.Major, 64)
	fv, err := strconv.ParseFloat(regex.ReplaceAllString(ver.Minor, ""), 64)
	if err != nil {
		lg.Warn("failed to parse version", zap.String("ver", fmt.Sprintf("%+v", ver)), zap.Error(err))
		return ServerVersionInfo{}, err
	}
	ver.VersionValue += (fv * 0.01)

	lg.Info("fetched version", zap.String("version", fmt.Sprintf("%+v", ver)))
	return ver, nil
}

func (e *eks) FetchSupportedAPIGroupVersions() (float64, map[string]struct{}, error) {
	vv, m, err := e.fetchSupportedAPIGroupVersions()
	return vv, m, err
}

func (e *eks) fetchSupportedAPIGroupVersions() (float64, map[string]struct{}, error) {
	if len(e.clients) == 0 {
		return 0.0, nil, errors.New("nil client")
	}
	ver, err := e.fetchServerVersion()
	if err != nil {
		return 0.0, nil, fmt.Errorf("failed to check api-resources because version check failed (%v)", err)
	}
	vv := ver.VersionValue

	dc := e.getClient().Discovery()

	e.cfg.Logger.Info("listing supported api-resources from kube-apiserver", zap.Float64("version-value", vv))
	groupList, err := dc.ServerGroups() // returns the supported groups
	if err != nil {
		return vv, nil, fmt.Errorf("failed to get server groups (%v)", err)
	}
	apiVersions := metav1.ExtractGroupVersions(groupList)

	m := make(map[string]struct{})
	for _, k := range apiVersions {
		m[k] = struct{}{}
	}
	return vv, m, nil
}

func (e *eks) ListNamespaces(batchLimit int64, batchInterval time.Duration) ([]v1.Namespace, error) {
	ns, err := e.listNamespaces(batchLimit, batchInterval)
	return ns, err
}

func (e *eks) listNamespaces(batchLimit int64, batchInterval time.Duration) (ns []v1.Namespace, err error) {
	rs := &v1.NamespaceList{ListMeta: metav1.ListMeta{Continue: ""}}
	for {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		rs, err = e.getClient().CoreV1().Namespaces().List(ctx, metav1.ListOptions{Limit: batchLimit, Continue: rs.Continue})
		cancel()
		if err != nil {
			return nil, err
		}
		ns = append(ns, rs.Items...)
		remained := int64Value(rs.RemainingItemCount)
		e.cfg.Logger.Info("listing namespace",
			zap.Int64("batch-limit", batchLimit),
			zap.Int64("remained", remained),
			zap.String("continue", rs.Continue),
			zap.Duration("batch-interval", batchInterval),
			zap.Int("items", len(rs.Items)),
		)
		if rs.Continue == "" {
			break
		}
		time.Sleep(batchInterval)
	}
	return ns, err
}

func (e *eks) ListNodes(batchLimit int64, batchInterval time.Duration) ([]v1.Node, error) {
	ns, err := e.listNodes(batchLimit, batchInterval)
	return ns, err
}

func (e *eks) listNodes(batchLimit int64, batchInterval time.Duration) (nodes []v1.Node, err error) {
	rs := &v1.NodeList{ListMeta: metav1.ListMeta{Continue: ""}}
	for {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		rs, err = e.getClient().CoreV1().Nodes().List(ctx, metav1.ListOptions{Limit: batchLimit, Continue: rs.Continue})
		cancel()
		if err != nil {
			return nil, err
		}
		nodes = append(nodes, rs.Items...)
		remained := int64Value(rs.RemainingItemCount)
		e.cfg.Logger.Info("listing nodes",
			zap.Int64("batch-limit", batchLimit),
			zap.Int64("remained", remained),
			zap.String("continue", rs.Continue),
			zap.Duration("batch-interval", batchInterval),
			zap.Int("items", len(rs.Items)),
		)
		if rs.Continue == "" {
			break
		}
		time.Sleep(batchInterval)
	}
	return nodes, err
}

func (e *eks) ListPods(namespace string, batchLimit int64, batchInterval time.Duration) ([]v1.Pod, error) {
	ns, err := e.listPods(namespace, batchLimit, batchInterval)
	return ns, err
}

func (e *eks) listPods(namespace string, batchLimit int64, batchInterval time.Duration) (pods []v1.Pod, err error) {
	rs := &v1.PodList{ListMeta: metav1.ListMeta{Continue: ""}}
	for {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		rs, err = e.getClient().CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{Limit: batchLimit, Continue: rs.Continue})
		cancel()
		if err != nil {
			return nil, err
		}
		pods = append(pods, rs.Items...)
		remained := int64Value(rs.RemainingItemCount)
		e.cfg.Logger.Info("listing pods",
			zap.String("namespace", namespace),
			zap.Int64("batch-limit", batchLimit),
			zap.Int64("remained", remained),
			zap.String("continue", rs.Continue),
			zap.Duration("batch-interval", batchInterval),
			zap.Int("items", len(rs.Items)),
		)
		if rs.Continue == "" {
			break
		}
		time.Sleep(batchInterval)
	}
	return pods, err
}

func (e *eks) ListSecrets(namespace string, batchLimit int64, batchInterval time.Duration) ([]v1.Secret, error) {
	ss, err := e.listSecrets(namespace, batchLimit, batchInterval)
	return ss, err
}

func (e *eks) listSecrets(namespace string, batchLimit int64, batchInterval time.Duration) (ss []v1.Secret, err error) {
	rs := &v1.SecretList{ListMeta: metav1.ListMeta{Continue: ""}}
	for {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		rs, err = e.getClient().CoreV1().Secrets(namespace).List(ctx, metav1.ListOptions{Limit: batchLimit, Continue: rs.Continue})
		cancel()
		if err != nil {
			return nil, err
		}
		ss = append(ss, rs.Items...)
		remained := int64Value(rs.RemainingItemCount)
		e.cfg.Logger.Info("listing secret",
			zap.Int64("batch-limit", batchLimit),
			zap.Int64("remained", remained),
			zap.String("continue", rs.Continue),
			zap.Duration("batch-interval", batchInterval),
			zap.Int("items", len(rs.Items)),
		)
		if rs.Continue == "" {
			break
		}
		time.Sleep(batchInterval)
	}
	return ss, err
}

func (e *eks) ListAppsV1Deployments(namespace string, batchLimit int64, batchInterval time.Duration) (ss []apps_v1.Deployment, err error) {
	rs := &apps_v1.DeploymentList{ListMeta: metav1.ListMeta{Continue: ""}}
	for {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		rs, err = e.getClient().AppsV1().Deployments(namespace).List(ctx, metav1.ListOptions{Limit: batchLimit, Continue: rs.Continue})
		cancel()
		if err != nil {
			return nil, err
		}
		ss = append(ss, rs.Items...)
		remained := int64Value(rs.RemainingItemCount)
		e.cfg.Logger.Info("listing deployments apps/v1",
			zap.String("namespace", namespace),
			zap.Int64("batch-limit", batchLimit),
			zap.Int64("remained", remained),
			zap.String("continue", rs.Continue),
			zap.Duration("batch-interval", batchInterval),
			zap.Int("items", len(rs.Items)),
		)
		if rs.Continue == "" {
			break
		}
		time.Sleep(batchInterval)
	}
	for i := range ss {
		if ss[i].TypeMeta.APIVersion == "" {
			ss[i].TypeMeta.APIVersion = "apps/v1"
		}
		if ss[i].TypeMeta.Kind == "" {
			ss[i].TypeMeta.Kind = "Deployment"
		}
		if ss[i].ObjectMeta.Namespace == "" {
			ss[i].ObjectMeta.Namespace = namespace
		}
	}
	return ss, err
}

func (e *eks) ListAppsV1StatefulSets(namespace string, batchLimit int64, batchInterval time.Duration) (ss []apps_v1.StatefulSet, err error) {
	rs := &apps_v1.StatefulSetList{ListMeta: metav1.ListMeta{Continue: ""}}
	for {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		rs, err = e.getClient().AppsV1().StatefulSets(namespace).List(ctx, metav1.ListOptions{Limit: batchLimit, Continue: rs.Continue})
		cancel()
		if err != nil {
			return nil, err
		}
		ss = append(ss, rs.Items...)
		remained := int64Value(rs.RemainingItemCount)
		e.cfg.Logger.Info("listing statefulsets apps/v1",
			zap.String("namespace", namespace),
			zap.Int64("batch-limit", batchLimit),
			zap.Int64("remained", remained),
			zap.String("continue", rs.Continue),
			zap.Duration("batch-interval", batchInterval),
			zap.Int("items", len(rs.Items)),
		)
		if rs.Continue == "" {
			break
		}
		time.Sleep(batchInterval)
	}
	for i := range ss {
		if ss[i].TypeMeta.APIVersion == "" {
			ss[i].TypeMeta.APIVersion = "apps/v1"
		}
		if ss[i].TypeMeta.Kind == "" {
			ss[i].TypeMeta.Kind = "StatefulSet"
		}
		if ss[i].ObjectMeta.Namespace == "" {
			ss[i].ObjectMeta.Namespace = namespace
		}
	}
	return ss, err
}

func (e *eks) ListAppsV1DaemonSets(namespace string, batchLimit int64, batchInterval time.Duration) (ss []apps_v1.DaemonSet, err error) {
	rs := &apps_v1.DaemonSetList{ListMeta: metav1.ListMeta{Continue: ""}}
	for {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		rs, err = e.getClient().AppsV1().DaemonSets(namespace).List(ctx, metav1.ListOptions{Limit: batchLimit, Continue: rs.Continue})
		cancel()
		if err != nil {
			return nil, err
		}
		ss = append(ss, rs.Items...)
		remained := int64Value(rs.RemainingItemCount)
		e.cfg.Logger.Info("listing daemonsets apps/v1",
			zap.String("namespace", namespace),
			zap.Int64("batch-limit", batchLimit),
			zap.Int64("remained", remained),
			zap.String("continue", rs.Continue),
			zap.Duration("batch-interval", batchInterval),
			zap.Int("items", len(rs.Items)),
		)
		if rs.Continue == "" {
			break
		}
		time.Sleep(batchInterval)
	}
	for i := range ss {
		if ss[i].TypeMeta.APIVersion == "" {
			ss[i].TypeMeta.APIVersion = "apps/v1"
		}
		if ss[i].TypeMeta.Kind == "" {
			ss[i].TypeMeta.Kind = "DaemonSet"
		}
		if ss[i].ObjectMeta.Namespace == "" {
			ss[i].ObjectMeta.Namespace = namespace
		}
	}
	return ss, err
}

func (e *eks) ListAppsV1ReplicaSets(namespace string, batchLimit int64, batchInterval time.Duration) (ss []apps_v1.ReplicaSet, err error) {
	rs := &apps_v1.ReplicaSetList{ListMeta: metav1.ListMeta{Continue: ""}}
	for {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		rs, err = e.getClient().AppsV1().ReplicaSets(namespace).List(ctx, metav1.ListOptions{Limit: batchLimit, Continue: rs.Continue})
		cancel()
		if err != nil {
			return nil, err
		}
		ss = append(ss, rs.Items...)
		remained := int64Value(rs.RemainingItemCount)
		e.cfg.Logger.Info("listing replicasets apps/v1",
			zap.String("namespace", namespace),
			zap.Int64("batch-limit", batchLimit),
			zap.Int64("remained", remained),
			zap.String("continue", rs.Continue),
			zap.Duration("batch-interval", batchInterval),
			zap.Int("items", len(rs.Items)),
		)
		if rs.Continue == "" {
			break
		}
		time.Sleep(batchInterval)
	}
	for i := range ss {
		if ss[i].TypeMeta.APIVersion == "" {
			ss[i].TypeMeta.APIVersion = "apps/v1"
		}
		if ss[i].TypeMeta.Kind == "" {
			ss[i].TypeMeta.Kind = "ReplicaSet"
		}
		if ss[i].ObjectMeta.Namespace == "" {
			ss[i].ObjectMeta.Namespace = namespace
		}
	}
	return ss, err
}

func (e *eks) ListNetworkingV1NetworkPolicies(namespace string, batchLimit int64, batchInterval time.Duration) (ss []networking_v1.NetworkPolicy, err error) {
	rs := &networking_v1.NetworkPolicyList{ListMeta: metav1.ListMeta{Continue: ""}}
	for {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		rs, err = e.getClient().NetworkingV1().NetworkPolicies(namespace).List(ctx, metav1.ListOptions{Limit: batchLimit, Continue: rs.Continue})
		cancel()
		if err != nil {
			return nil, err
		}
		ss = append(ss, rs.Items...)
		remained := int64Value(rs.RemainingItemCount)
		e.cfg.Logger.Info("listing networkpolicies networking.k8s.io/v1",
			zap.String("namespace", namespace),
			zap.Int64("batch-limit", batchLimit),
			zap.Int64("remained", remained),
			zap.String("continue", rs.Continue),
			zap.Duration("batch-interval", batchInterval),
			zap.Int("items", len(rs.Items)),
		)
		if rs.Continue == "" {
			break
		}
		time.Sleep(batchInterval)
	}
	for i := range ss {
		if ss[i].TypeMeta.APIVersion == "" {
			ss[i].TypeMeta.APIVersion = "networking.k8s.io/v1"
		}
		if ss[i].TypeMeta.Kind == "" {
			ss[i].TypeMeta.Kind = "NetworkPolicy"
		}
		if ss[i].ObjectMeta.Namespace == "" {
			ss[i].ObjectMeta.Namespace = namespace
		}
	}
	return ss, err
}

func (e *eks) ListPolicyV1beta1PodSecurityPolicies(batchLimit int64, batchInterval time.Duration) (ss []policy_v1beta1.PodSecurityPolicy, err error) {
	rs := &policy_v1beta1.PodSecurityPolicyList{ListMeta: metav1.ListMeta{Continue: ""}}
	for {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		rs, err = e.getClient().PolicyV1beta1().PodSecurityPolicies().List(ctx, metav1.ListOptions{Limit: batchLimit, Continue: rs.Continue})
		cancel()
		if err != nil {
			return nil, err
		}
		ss = append(ss, rs.Items...)
		remained := int64Value(rs.RemainingItemCount)
		e.cfg.Logger.Info("listing podsecuritypolicies policy/v1beta1",
			zap.Int64("batch-limit", batchLimit),
			zap.Int64("remained", remained),
			zap.String("continue", rs.Continue),
			zap.Duration("batch-interval", batchInterval),
			zap.Int("items", len(rs.Items)),
		)
		if rs.Continue == "" {
			break
		}
		time.Sleep(batchInterval)
	}
	for i := range ss {
		if ss[i].TypeMeta.APIVersion == "" {
			ss[i].TypeMeta.APIVersion = "policy/v1beta1"
		}
		if ss[i].TypeMeta.Kind == "" {
			ss[i].TypeMeta.Kind = "PodSecurityPolicy"
		}
	}
	return ss, err
}

func (e *eks) ListAppsV1beta1Deployments(namespace string, batchLimit int64, batchInterval time.Duration) (ss []apps_v1beta1.Deployment, err error) {
	rs := &apps_v1beta1.DeploymentList{ListMeta: metav1.ListMeta{Continue: ""}}
	for {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		rs, err = e.getClient().AppsV1beta1().Deployments(namespace).List(ctx, metav1.ListOptions{Limit: batchLimit, Continue: rs.Continue})
		cancel()
		if err != nil {
			return nil, err
		}
		ss = append(ss, rs.Items...)
		remained := int64Value(rs.RemainingItemCount)
		e.cfg.Logger.Info("listing deployments apps/v1beta1",
			zap.String("namespace", namespace),
			zap.Int64("batch-limit", batchLimit),
			zap.Int64("remained", remained),
			zap.String("continue", rs.Continue),
			zap.Duration("batch-interval", batchInterval),
			zap.Int("items", len(rs.Items)),
		)
		if rs.Continue == "" {
			break
		}
		time.Sleep(batchInterval)
	}
	for i := range ss {
		if ss[i].TypeMeta.APIVersion == "" {
			ss[i].TypeMeta.APIVersion = "apps/v1beta1"
		}
		if ss[i].TypeMeta.Kind == "" {
			ss[i].TypeMeta.Kind = "Deployment"
		}
		if ss[i].ObjectMeta.Namespace == "" {
			ss[i].ObjectMeta.Namespace = namespace
		}
	}
	return ss, err
}

func (e *eks) ListAppsV1beta1StatefulSets(namespace string, batchLimit int64, batchInterval time.Duration) (ss []apps_v1beta1.StatefulSet, err error) {
	rs := &apps_v1beta1.StatefulSetList{ListMeta: metav1.ListMeta{Continue: ""}}
	for {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		rs, err = e.getClient().AppsV1beta1().StatefulSets(namespace).List(ctx, metav1.ListOptions{Limit: batchLimit, Continue: rs.Continue})
		cancel()
		if err != nil {
			return nil, err
		}
		ss = append(ss, rs.Items...)
		remained := int64Value(rs.RemainingItemCount)
		e.cfg.Logger.Info("listing statefulsets apps/v1beta1",
			zap.String("namespace", namespace),
			zap.Int64("batch-limit", batchLimit),
			zap.Int64("remained", remained),
			zap.String("continue", rs.Continue),
			zap.Duration("batch-interval", batchInterval),
			zap.Int("items", len(rs.Items)),
		)
		if rs.Continue == "" {
			break
		}
		time.Sleep(batchInterval)
	}
	for i := range ss {
		if ss[i].TypeMeta.APIVersion == "" {
			ss[i].TypeMeta.APIVersion = "apps/v1beta1"
		}
		if ss[i].TypeMeta.Kind == "" {
			ss[i].TypeMeta.Kind = "StatefulSet"
		}
		if ss[i].ObjectMeta.Namespace == "" {
			ss[i].ObjectMeta.Namespace = namespace
		}
	}
	return ss, err
}

func (e *eks) ListAppsV1beta2Deployments(namespace string, batchLimit int64, batchInterval time.Duration) (ss []apps_v1beta2.Deployment, err error) {
	rs := &apps_v1beta2.DeploymentList{ListMeta: metav1.ListMeta{Continue: ""}}
	for {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		rs, err = e.getClient().AppsV1beta2().Deployments(namespace).List(ctx, metav1.ListOptions{Limit: batchLimit, Continue: rs.Continue})
		cancel()
		if err != nil {
			return nil, err
		}
		ss = append(ss, rs.Items...)
		remained := int64Value(rs.RemainingItemCount)
		e.cfg.Logger.Info("listing deployments apps/v1beta2",
			zap.String("namespace", namespace),
			zap.Int64("batch-limit", batchLimit),
			zap.Int64("remained", remained),
			zap.String("continue", rs.Continue),
			zap.Duration("batch-interval", batchInterval),
			zap.Int("items", len(rs.Items)),
		)
		if rs.Continue == "" {
			break
		}
		time.Sleep(batchInterval)
	}
	for i := range ss {
		if ss[i].TypeMeta.APIVersion == "" {
			ss[i].TypeMeta.APIVersion = "apps/v1beta2"
		}
		if ss[i].TypeMeta.Kind == "" {
			ss[i].TypeMeta.Kind = "Deployment"
		}
		if ss[i].ObjectMeta.Namespace == "" {
			ss[i].ObjectMeta.Namespace = namespace
		}
	}
	return ss, err
}

func (e *eks) ListAppsV1beta2StatefulSets(namespace string, batchLimit int64, batchInterval time.Duration) (ss []apps_v1beta2.StatefulSet, err error) {
	rs := &apps_v1beta2.StatefulSetList{ListMeta: metav1.ListMeta{Continue: ""}}
	for {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		rs, err = e.getClient().AppsV1beta2().StatefulSets(namespace).List(ctx, metav1.ListOptions{Limit: batchLimit, Continue: rs.Continue})
		cancel()
		if err != nil {
			return nil, err
		}
		ss = append(ss, rs.Items...)
		remained := int64Value(rs.RemainingItemCount)
		e.cfg.Logger.Info("listing statefulsets apps/v1beta2",
			zap.String("namespace", namespace),
			zap.Int64("batch-limit", batchLimit),
			zap.Int64("remained", remained),
			zap.String("continue", rs.Continue),
			zap.Duration("batch-interval", batchInterval),
			zap.Int("items", len(rs.Items)),
		)
		if rs.Continue == "" {
			break
		}
		time.Sleep(batchInterval)
	}
	for i := range ss {
		if ss[i].TypeMeta.APIVersion == "" {
			ss[i].TypeMeta.APIVersion = "apps/v1beta2"
		}
		if ss[i].TypeMeta.Kind == "" {
			ss[i].TypeMeta.Kind = "StatefulSet"
		}
		if ss[i].ObjectMeta.Namespace == "" {
			ss[i].ObjectMeta.Namespace = namespace
		}
	}
	return ss, err
}

func (e *eks) ListExtensionsV1beta1DaemonSets(namespace string, batchLimit int64, batchInterval time.Duration) (ss []extensions_v1beta1.DaemonSet, err error) {
	rs := &extensions_v1beta1.DaemonSetList{ListMeta: metav1.ListMeta{Continue: ""}}
	for {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		rs, err = e.getClient().ExtensionsV1beta1().DaemonSets(namespace).List(ctx, metav1.ListOptions{Limit: batchLimit, Continue: rs.Continue})
		cancel()
		if err != nil {
			return nil, err
		}
		ss = append(ss, rs.Items...)
		remained := int64Value(rs.RemainingItemCount)
		e.cfg.Logger.Info("listing daemonsets extensions/v1beta1",
			zap.String("namespace", namespace),
			zap.Int64("batch-limit", batchLimit),
			zap.Int64("remained", remained),
			zap.String("continue", rs.Continue),
			zap.Duration("batch-interval", batchInterval),
			zap.Int("items", len(rs.Items)),
		)
		if rs.Continue == "" {
			break
		}
		time.Sleep(batchInterval)
	}
	for i := range ss {
		if ss[i].TypeMeta.APIVersion == "" {
			ss[i].TypeMeta.APIVersion = "extensions/v1beta1"
		}
		if ss[i].TypeMeta.Kind == "" {
			ss[i].TypeMeta.Kind = "DaemonSet"
		}
		if ss[i].ObjectMeta.Namespace == "" {
			ss[i].ObjectMeta.Namespace = namespace
		}
	}
	return ss, err
}

func (e *eks) ListExtensionsV1beta1Deployments(namespace string, batchLimit int64, batchInterval time.Duration) (ss []extensions_v1beta1.Deployment, err error) {
	rs := &extensions_v1beta1.DeploymentList{ListMeta: metav1.ListMeta{Continue: ""}}
	for {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		rs, err = e.getClient().ExtensionsV1beta1().Deployments(namespace).List(ctx, metav1.ListOptions{Limit: batchLimit, Continue: rs.Continue})
		cancel()
		if err != nil {
			return nil, err
		}
		ss = append(ss, rs.Items...)
		remained := int64Value(rs.RemainingItemCount)
		e.cfg.Logger.Info("listing deployments extensions/v1beta1",
			zap.String("namespace", namespace),
			zap.Int64("batch-limit", batchLimit),
			zap.Int64("remained", remained),
			zap.String("continue", rs.Continue),
			zap.Duration("batch-interval", batchInterval),
			zap.Int("items", len(rs.Items)),
		)
		if rs.Continue == "" {
			break
		}
		time.Sleep(batchInterval)
	}
	for i := range ss {
		if ss[i].TypeMeta.APIVersion == "" {
			ss[i].TypeMeta.APIVersion = "extensions/v1beta1"
		}
		if ss[i].TypeMeta.Kind == "" {
			ss[i].TypeMeta.Kind = "Deployment"
		}
		if ss[i].ObjectMeta.Namespace == "" {
			ss[i].ObjectMeta.Namespace = namespace
		}
	}
	return ss, err
}

func (e *eks) ListExtensionsV1beta1ReplicaSets(namespace string, batchLimit int64, batchInterval time.Duration) (ss []extensions_v1beta1.ReplicaSet, err error) {
	rs := &extensions_v1beta1.ReplicaSetList{ListMeta: metav1.ListMeta{Continue: ""}}
	for {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		rs, err = e.getClient().ExtensionsV1beta1().ReplicaSets(namespace).List(ctx, metav1.ListOptions{Limit: batchLimit, Continue: rs.Continue})
		cancel()
		if err != nil {
			return nil, err
		}
		ss = append(ss, rs.Items...)
		remained := int64Value(rs.RemainingItemCount)
		e.cfg.Logger.Info("listing replicasets extensions/v1beta1",
			zap.String("namespace", namespace),
			zap.Int64("batch-limit", batchLimit),
			zap.Int64("remained", remained),
			zap.String("continue", rs.Continue),
			zap.Duration("batch-interval", batchInterval),
			zap.Int("items", len(rs.Items)),
		)
		if rs.Continue == "" {
			break
		}
		time.Sleep(batchInterval)
	}
	for i := range ss {
		if ss[i].TypeMeta.APIVersion == "" {
			ss[i].TypeMeta.APIVersion = "extensions/v1beta1"
		}
		if ss[i].TypeMeta.Kind == "" {
			ss[i].TypeMeta.Kind = "ReplicaSet"
		}
		if ss[i].ObjectMeta.Namespace == "" {
			ss[i].ObjectMeta.Namespace = namespace
		}
	}
	return ss, err
}

func (e *eks) ListExtensionsV1beta1NetworkPolicies(namespace string, batchLimit int64, batchInterval time.Duration) (ss []extensions_v1beta1.NetworkPolicy, err error) {
	rs := &extensions_v1beta1.NetworkPolicyList{ListMeta: metav1.ListMeta{Continue: ""}}
	for {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		rs, err = e.getClient().ExtensionsV1beta1().NetworkPolicies(namespace).List(ctx, metav1.ListOptions{Limit: batchLimit, Continue: rs.Continue})
		cancel()
		if err != nil {
			return nil, err
		}
		ss = append(ss, rs.Items...)
		remained := int64Value(rs.RemainingItemCount)
		e.cfg.Logger.Info("listing networkpolicies extensions/v1beta1",
			zap.String("namespace", namespace),
			zap.Int64("batch-limit", batchLimit),
			zap.Int64("remained", remained),
			zap.String("continue", rs.Continue),
			zap.Duration("batch-interval", batchInterval),
			zap.Int("items", len(rs.Items)),
		)
		if rs.Continue == "" {
			break
		}
		time.Sleep(batchInterval)
	}
	for i := range ss {
		if ss[i].TypeMeta.APIVersion == "" {
			ss[i].TypeMeta.APIVersion = "extensions/v1beta1"
		}
		if ss[i].TypeMeta.Kind == "" {
			ss[i].TypeMeta.Kind = "NetworkPolicy"
		}
		if ss[i].ObjectMeta.Namespace == "" {
			ss[i].ObjectMeta.Namespace = namespace
		}
	}
	return ss, err
}

func (e *eks) ListExtensionsV1beta1PodSecurityPolicies(batchLimit int64, batchInterval time.Duration) (ss []extensions_v1beta1.PodSecurityPolicy, err error) {
	rs := &extensions_v1beta1.PodSecurityPolicyList{ListMeta: metav1.ListMeta{Continue: ""}}
	for {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		rs, err = e.getClient().ExtensionsV1beta1().PodSecurityPolicies().List(ctx, metav1.ListOptions{Limit: batchLimit, Continue: rs.Continue})
		cancel()
		if err != nil {
			return nil, err
		}
		ss = append(ss, rs.Items...)
		remained := int64Value(rs.RemainingItemCount)
		e.cfg.Logger.Info("listing podsecuritypolicies extensions/v1beta1",
			zap.Int64("batch-limit", batchLimit),
			zap.Int64("remained", remained),
			zap.String("continue", rs.Continue),
			zap.Duration("batch-interval", batchInterval),
			zap.Int("items", len(rs.Items)),
		)
		if rs.Continue == "" {
			break
		}
		time.Sleep(batchInterval)
	}
	for i := range ss {
		if ss[i].TypeMeta.APIVersion == "" {
			ss[i].TypeMeta.APIVersion = "extensions/v1beta1"
		}
		if ss[i].TypeMeta.Kind == "" {
			ss[i].TypeMeta.Kind = "PodSecurityPolicy"
		}
	}
	return ss, err
}

func int64Value(p *int64) int64 {
	if p == nil {
		return 0
	}
	return *p
}

// Object contains all object metadata.
type Object struct {
	// Kind is a string value representing the REST resource this object represents.
	// Servers may infer this from the endpoint the client submits requests to.
	// Cannot be updated.
	// In CamelCase.
	// More info: https://git.k8s.io/community/contributors/devel/sig-architecture/api-conventions.md#types-kinds
	// ref. metav1.TypeMeta
	Kind string `json:"kind"`
	// APIVersion defines the versioned schema of this representation of an object.
	// Servers should convert recognized schemas to the latest internal value, and
	// may reject unrecognized values.
	// More info: https://git.k8s.io/community/contributors/devel/sig-architecture/api-conventions.md#resources
	// ref. metav1.TypeMeta
	APIVersion string `json:"apiVersion"`

	ObjectMeta metav1.ObjectMeta `json:"metadata"`
}

func (e *eks) GetObject(namespace string, kind string, name string) (obj Object, d []byte, err error) {
	if e.cfg.KubectlPath == "" {
		return Object{}, nil, errors.New("empty EKSConfig.KubectlPath")
	}
	if e.cfg.KubeConfigPath == "" {
		return Object{}, nil, errors.New("empty EKSConfig.KubeConfigPath")
	}
	if !fileutil.Exist(e.cfg.KubeConfigPath) {
		return Object{}, nil, fmt.Errorf("%q not found", e.cfg.KubeConfigPath)
	}
	if !fileutil.Exist(e.cfg.KubectlPath) {
		return Object{}, nil, fmt.Errorf("%q not found", e.cfg.KubectlPath)
	}
	if err := fileutil.EnsureExecutable(e.cfg.KubectlPath); err != nil {
		return Object{}, nil, fmt.Errorf("cannot execute %q (%v)", e.cfg.KubectlPath, err)
	}

	if kind == "" {
		return Object{}, nil, fmt.Errorf("empty Kind for %q", name)
	}
	if name == "" {
		return Object{}, nil, errors.New("empty name")
	}

	args := []string{
		e.cfg.KubectlPath,
		"--kubeconfig=" + e.cfg.KubeConfigPath,
	}
	if namespace != "" {
		args = append(args, "--namespace="+namespace)
	}
	args = append(args,
		"get",
		kind,
		name,
		"-o=yaml",
	)

	e.cfg.Logger.Info("running kubectl get",
		zap.String("namespace", namespace),
		zap.String("kind", kind),
		zap.String("name", name),
	)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	output, err := exec.New().CommandContext(ctx, args[0], args[1:]...).CombinedOutput()
	cancel()
	out := strings.TrimSpace(string(output))
	if err != nil {
		return Object{}, nil, fmt.Errorf("'kubectl get' failed %v (output %q)", err, out)
	}

	if err = yaml.Unmarshal([]byte(out), &obj); err != nil {
		return Object{}, nil, err
	}
	if obj.Kind == "" {
		obj.Kind = kind
	}
	if obj.ObjectMeta.Namespace == "" && namespace != "" {
		obj.ObjectMeta.Namespace = namespace
	}
	if obj.ObjectMeta.Name == "" {
		obj.ObjectMeta.Name = name
	}
	return obj, []byte(out), nil
}