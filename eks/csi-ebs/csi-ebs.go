// Package csiebs installs "aws-ebs-csi-driver".
// ref. https://github.com/kubernetes-sigs/aws-ebs-csi-driver
// ref. https://github.com/kubernetes-sigs/aws-ebs-csi-driver/blob/master/aws-ebs-csi-driver/values.yaml
package csiebs

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/aws/aws-k8s-tester/eks/helm"
	"github.com/aws/aws-k8s-tester/eksconfig"
	k8s_client "github.com/aws/aws-k8s-tester/pkg/k8s-client"
	"github.com/aws/aws-k8s-tester/pkg/timeutil"
	"go.uber.org/zap"
	"k8s.io/utils/exec"
)

// Config defines AWS EBS CSI Driver configuration.
type Config struct {
	Logger *zap.Logger
	Stopc  chan struct{}

	EKSConfig *eksconfig.Config
	K8SClient k8s_client.EKS
}

// Tester defines AWS EBS CSI Driver tester
type Tester interface {
	// Create installs AWS EBS CSI Driver.
	Create() error
	// Delete deletes AWS EBS CSI Driver.
	Delete() error
}

func New(cfg Config) (Tester, error) {
	return &tester{cfg: cfg}, nil
}

type tester struct {
	cfg Config
}

const chartName = "aws-ebs-csi-driver"

func (ts *tester) Create() error {
	if ts.cfg.EKSConfig.AddOnCSIEBS.Created {
		ts.cfg.Logger.Info("skipping create AddOnCSIEBS")
		return nil
	}

	ts.cfg.EKSConfig.AddOnCSIEBS.Created = true
	ts.cfg.EKSConfig.Sync()
	createStart := time.Now()
	defer func() {
		createEnd := time.Now()
		ts.cfg.EKSConfig.AddOnCSIEBS.TimeFrameCreate = timeutil.NewTimeFrame(createStart, createEnd)
		ts.cfg.EKSConfig.Sync()
	}()

	if err := ts.createHelmCSI(); err != nil {
		return err
	}

	return ts.cfg.EKSConfig.Sync()
}

func (ts *tester) Delete() error {
	if !ts.cfg.EKSConfig.AddOnCSIEBS.Created {
		ts.cfg.Logger.Info("skipping delete AddOnCSIEBS")
		return nil
	}

	deleteStart := time.Now()
	defer func() {
		deleteEnd := time.Now()
		ts.cfg.EKSConfig.AddOnCSIEBS.TimeFrameDelete = timeutil.NewTimeFrame(deleteStart, deleteEnd)
		ts.cfg.EKSConfig.Sync()
	}()

	var errs []string

	if err := ts.deleteHelmCSI(); err != nil {
		errs = append(errs, err.Error())
	}

	if len(errs) > 0 {
		return errors.New(strings.Join(errs, ", "))
	}

	ts.cfg.EKSConfig.AddOnCSIEBS.Created = false
	return ts.cfg.EKSConfig.Sync()
}

// https://github.com/kubernetes-sigs/aws-ebs-csi-driver/blob/master/aws-ebs-csi-driver/values.yaml
func (ts *tester) createHelmCSI() error {
	values := map[string]interface{}{
		"enableVolumeScheduling": true,
		"enableVolumeResizing":   true,
		"enableVolumeSnapshot":   true,
	}

	getAllArgs := []string{
		ts.cfg.EKSConfig.KubectlPath,
		"--kubeconfig=" + ts.cfg.EKSConfig.KubeConfigPath,
		"--namespace=kube-system",
		"get",
		"all",
	}
	getAllCmd := strings.Join(getAllArgs, " ")

	descArgsDs := []string{
		ts.cfg.EKSConfig.KubectlPath,
		"--kubeconfig=" + ts.cfg.EKSConfig.KubeConfigPath,
		"--namespace=kube-system",
		"describe",
		"daemonset.apps/ebs-csi-node",
	}
	descCmdDs := strings.Join(descArgsDs, " ")

	descArgsDp := []string{
		ts.cfg.EKSConfig.KubectlPath,
		"--kubeconfig=" + ts.cfg.EKSConfig.KubeConfigPath,
		"--namespace=kube-system",
		"describe",
		"deployment.apps/ebs-csi-controller",
	}
	descCmdDp := strings.Join(descArgsDp, " ")

	descArgsPods := []string{
		ts.cfg.EKSConfig.KubectlPath,
		"--kubeconfig=" + ts.cfg.EKSConfig.KubeConfigPath,
		"--namespace=kube-system",
		"describe",
		"pods",
		"--selector=app=ebs-csi-node",
	}
	descCmdPods := strings.Join(descArgsPods, " ")

	logArgs := []string{
		ts.cfg.EKSConfig.KubectlPath,
		"--kubeconfig=" + ts.cfg.EKSConfig.KubeConfigPath,
		"--namespace=kube-system",
		"logs",
		"--selector=app=ebs-csi-node",
		"--all-containers=true",
	}
	logsCmd := strings.Join(logArgs, " ")

	return helm.Install(helm.InstallConfig{
		Logger:         ts.cfg.Logger,
		Stopc:          ts.cfg.Stopc,
		Timeout:        15 * time.Minute,
		KubeConfigPath: ts.cfg.EKSConfig.KubeConfigPath,
		Namespace:      "kube-system",
		ChartRepoURL:   ts.cfg.EKSConfig.AddOnCSIEBS.ChartRepoURL,
		ChartName:      chartName,
		ReleaseName:    chartName,
		Values:         values,
		LogFunc: func(format string, v ...interface{}) {
			ts.cfg.Logger.Info(fmt.Sprintf("[install] "+format, v...))
		},
		QueryFunc: func() {
			println()

			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			output, err := exec.New().CommandContext(ctx, getAllArgs[0], getAllArgs[1:]...).CombinedOutput()
			cancel()
			out := strings.TrimSpace(string(output))
			if err != nil {
				ts.cfg.Logger.Warn("'kubectl get all' failed", zap.Error(err))
			} else {
				fmt.Printf("\n\n'%s' output:\n\n%s\n\n", getAllCmd, out)
			}

			ctx, cancel = context.WithTimeout(context.Background(), 15*time.Second)
			output, err = exec.New().CommandContext(ctx, descArgsDs[0], descArgsDs[1:]...).CombinedOutput()
			cancel()
			out = strings.TrimSpace(string(output))
			if err != nil {
				ts.cfg.Logger.Warn("'kubectl describe daemonset' failed", zap.Error(err))
			} else {
				fmt.Printf("\n\n'%s' output:\n\n%s\n\n", descCmdDs, out)
			}

			ctx, cancel = context.WithTimeout(context.Background(), 15*time.Second)
			output, err = exec.New().CommandContext(ctx, descArgsDp[0], descArgsDp[1:]...).CombinedOutput()
			cancel()
			out = strings.TrimSpace(string(output))
			if err != nil {
				ts.cfg.Logger.Warn("'kubectl describe deployment' failed", zap.Error(err))
			} else {
				fmt.Printf("\n\n'%s' output:\n\n%s\n\n", descCmdDp, out)
			}

			ctx, cancel = context.WithTimeout(context.Background(), 15*time.Second)
			output, err = exec.New().CommandContext(ctx, descArgsPods[0], descArgsPods[1:]...).CombinedOutput()
			cancel()
			out = strings.TrimSpace(string(output))
			if err != nil {
				ts.cfg.Logger.Warn("'kubectl describe pods' failed", zap.Error(err))
			} else {
				fmt.Printf("\n\n'%s' output:\n\n%s\n\n", descCmdPods, out)
			}

			ctx, cancel = context.WithTimeout(context.Background(), 15*time.Second)
			output, err = exec.New().CommandContext(ctx, logArgs[0], logArgs[1:]...).CombinedOutput()
			cancel()
			out = strings.TrimSpace(string(output))
			if err != nil {
				ts.cfg.Logger.Warn("'kubectl logs' failed", zap.Error(err))
			} else {
				fmt.Printf("\n\n'%s' output:\n\n%s\n\n", logsCmd, out)
			}
		},
		QueryInterval: 30 * time.Second,
	})
}

func (ts *tester) deleteHelmCSI() error {
	return helm.Uninstall(helm.InstallConfig{
		Logger:         ts.cfg.Logger,
		Timeout:        15 * time.Minute,
		KubeConfigPath: ts.cfg.EKSConfig.KubeConfigPath,
		Namespace:      "kube-system",
		ChartName:      chartName,
		ReleaseName:    chartName,
	})
}
