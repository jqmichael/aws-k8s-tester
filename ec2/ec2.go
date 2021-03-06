// Package ec2 implements testing utilities using EC2.
package ec2

import (
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/aws/aws-k8s-tester/ec2config"
	pkgaws "github.com/aws/aws-k8s-tester/pkg/aws"
	"github.com/aws/aws-k8s-tester/pkg/logutil"
	"github.com/aws/aws-k8s-tester/version"
	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/autoscaling"
	"github.com/aws/aws-sdk-go/service/autoscaling/autoscalingiface"
	"github.com/aws/aws-sdk-go/service/cloudformation"
	"github.com/aws/aws-sdk-go/service/cloudformation/cloudformationiface"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/ec2/ec2iface"
	"github.com/aws/aws-sdk-go/service/elbv2"
	"github.com/aws/aws-sdk-go/service/elbv2/elbv2iface"
	"github.com/aws/aws-sdk-go/service/iam"
	"github.com/aws/aws-sdk-go/service/iam/iamiface"
	"github.com/aws/aws-sdk-go/service/kms"
	"github.com/aws/aws-sdk-go/service/kms/kmsiface"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/aws/aws-sdk-go/service/s3/s3iface"
	"github.com/aws/aws-sdk-go/service/ssm"
	"github.com/aws/aws-sdk-go/service/ssm/ssmiface"
	"github.com/aws/aws-sdk-go/service/sts"
	"github.com/dustin/go-humanize"
	"go.uber.org/zap"
)

// Tester implements "kubetest2" Deployer.
// ref. https://github.com/kubernetes/test-infra/blob/master/kubetest2/pkg/types/types.go
type Tester struct {
	stopCreationCh     chan struct{}
	stopCreationChOnce *sync.Once

	osSig chan os.Signal

	downMu *sync.Mutex
	logsMu *sync.RWMutex

	lg  *zap.Logger
	cfg *ec2config.Config

	awsSession *session.Session
	iamAPI     iamiface.IAMAPI
	kmsAPI     kmsiface.KMSAPI
	ssmAPI     ssmiface.SSMAPI
	cfnAPI     cloudformationiface.CloudFormationAPI
	ec2API     ec2iface.EC2API
	s3API      s3iface.S3API
	asgAPI     autoscalingiface.AutoScalingAPI
	elbv2API   elbv2iface.ELBV2API
}

// New creates a new EC2 tester.
func New(cfg *ec2config.Config) (*Tester, error) {
	fmt.Println("😎 🙏 🚶 ✔️ 👍")
	fmt.Println(version.Version())
	fmt.Printf("\n*********************************\n")
	fmt.Printf("New %q\n", cfg.ConfigPath)
	if err := cfg.ValidateAndSetDefaults(); err != nil {
		return nil, err
	}

	lcfg := logutil.AddOutputPaths(logutil.GetDefaultZapLoggerConfig(), cfg.LogOutputs, cfg.LogOutputs)
	lcfg.Level = zap.NewAtomicLevelAt(logutil.ConvertToZapLevel(cfg.LogLevel))
	lg, err := lcfg.Build()
	if err != nil {
		return nil, err
	}

	ts := &Tester{
		stopCreationCh:     make(chan struct{}),
		stopCreationChOnce: new(sync.Once),
		osSig:              make(chan os.Signal),
		downMu:             new(sync.Mutex),
		logsMu:             new(sync.RWMutex),
		lg:                 lg,
		cfg:                cfg,
	}
	signal.Notify(ts.osSig, syscall.SIGTERM, syscall.SIGINT)

	defer ts.cfg.Sync()

	awsCfg := &pkgaws.Config{
		Logger:        ts.lg,
		DebugAPICalls: ts.cfg.LogLevel == "debug",
		Partition:     ts.cfg.Partition,
		Region:        ts.cfg.Region,
	}
	var stsOutput *sts.GetCallerIdentityOutput
	ts.awsSession, stsOutput, ts.cfg.AWSCredentialPath, err = pkgaws.New(awsCfg)
	if err != nil {
		return nil, err
	}
	ts.cfg.AWSAccountID = aws.StringValue(stsOutput.Account)
	ts.cfg.AWSUserID = aws.StringValue(stsOutput.UserId)
	ts.cfg.AWSIAMRoleARN = aws.StringValue(stsOutput.Arn)
	ts.cfg.Sync()

	ts.iamAPI = iam.New(ts.awsSession)
	ts.kmsAPI = kms.New(ts.awsSession)
	ts.ssmAPI = ssm.New(ts.awsSession)
	ts.cfnAPI = cloudformation.New(ts.awsSession)

	ts.ec2API = ec2.New(ts.awsSession)
	if _, err := ts.ec2API.DescribeInstances(&ec2.DescribeInstancesInput{MaxResults: aws.Int64(5)}); err != nil {
		return nil, fmt.Errorf("failed to describe instances using EC2 API (%v)", err)
	}
	fmt.Println("EC2 API available!")

	ts.s3API = s3.New(ts.awsSession)
	ts.asgAPI = autoscaling.New(ts.awsSession)
	ts.elbv2API = elbv2.New(ts.awsSession)

	return ts, nil
}

// Up should provision a new cluster for testing
func (ts *Tester) Up() (err error) {
	fmt.Printf("\n*********************************\n")
	ts.lg.Sugar().Infof("Up (%s)", ts.cfg.ConfigPath)

	now := time.Now()

	defer func() {
		fmt.Printf("\n*********************************\n")
		ts.lg.Sugar().Infof("Up.defer start (%s)", ts.cfg.ConfigPath)

		if serr := ts.uploadToS3(); serr != nil {
			ts.lg.Warn("failed to upload artifacts to S3", zap.Error(serr))
		}

		if err == nil {
			if ts.cfg.Up {
				fmt.Printf("\n*********************************\n")
				ts.lg.Sugar().Infof("SSH (%s)", ts.cfg.ConfigPath)
				fmt.Println(ts.cfg.SSHCommands())

				ts.lg.Info("Up succeeded",
					zap.String("started", humanize.RelTime(now, time.Now(), "ago", "from now")),
				)

				fmt.Printf("\n*********************************\n")
				ts.lg.Sugar().Infof("Up.defer end (%s)", ts.cfg.ConfigPath)
				fmt.Printf("\n\n💯 😁 👍 :) Up success\n\n\n")
			} else {
				fmt.Printf("\n\n😲 😲 aborted Up ???\n\n\n")
			}
			return
		}

		if !ts.cfg.OnFailureDelete {
			if ts.cfg.Up {
				fmt.Printf("\n*********************************\n")
				ts.lg.Sugar().Infof("SSH (%s)", ts.cfg.ConfigPath)
				fmt.Println(ts.cfg.SSHCommands())
			}

			ts.lg.Warn("Up failed",
				zap.String("started", humanize.RelTime(now, time.Now(), "ago", "from now")),
				zap.Error(err),
			)

			fmt.Printf("\n*********************************\n")
			ts.lg.Sugar().Infof("Up.defer end (%s)", ts.cfg.ConfigPath)
			fmt.Printf("\n\n🔥 💀 👽 😱 😡 (-_-) Up fail\n\n\n")
			return
		}

		if ts.cfg.Up {
			fmt.Printf("\n*********************************\n")
			ts.lg.Sugar().Infof("SSH (%s)", ts.cfg.ConfigPath)
			fmt.Println(ts.cfg.SSHCommands())
		}

		fmt.Printf("\n*********************************\n")
		fmt.Printf("🔥 💀 👽 😱 😡 (-_-) Up fail\n")
		ts.lg.Warn("Up failed; reverting resource creation",
			zap.String("started", humanize.RelTime(now, time.Now(), "ago", "from now")),
			zap.Error(err),
		)
		waitDur := time.Duration(ts.cfg.OnFailureDeleteWaitSeconds) * time.Second
		if waitDur > 0 {
			ts.lg.Info("waiting before clean up", zap.Duration("wait", waitDur))
			select {
			case <-ts.stopCreationCh:
				ts.lg.Info("wait aborted before clean up")
			case <-ts.osSig:
				ts.lg.Info("wait aborted before clean up")
			case <-time.After(waitDur):
			}
		}
		derr := ts.down()
		if derr != nil {
			ts.lg.Warn("failed to revert Up", zap.Error(derr))
		} else {
			ts.lg.Warn("reverted Up")
		}

		fmt.Printf("\n*********************************\n")
		ts.lg.Sugar().Infof("Up.defer end (%s)", ts.cfg.ConfigPath)
		fmt.Printf("\n\n🔥 💀 👽 😱 😡 (-_-) Up fail\n\n\n")
	}()

	ts.lg.Info("Up started",
		zap.String("version", version.Version()),
		zap.String("name", ts.cfg.Name),
	)
	defer ts.cfg.Sync()

	fmt.Printf("\n*********************************\n")
	fmt.Printf("createS3 (%q)\n", ts.cfg.ConfigPath)
	if err := catchInterrupt(
		ts.lg,
		ts.stopCreationCh,
		ts.stopCreationChOnce,
		ts.osSig,
		ts.createS3,
	); err != nil {
		return err
	}

	fmt.Printf("\n*********************************\n")
	fmt.Printf("createRole (%q)\n", ts.cfg.ConfigPath)
	if err := catchInterrupt(
		ts.lg,
		ts.stopCreationCh,
		ts.stopCreationChOnce,
		ts.osSig,
		ts.createRole,
	); err != nil {
		return err
	}

	fmt.Printf("\n*********************************\n")
	fmt.Printf("createVPC (%q)\n", ts.cfg.ConfigPath)
	if err := catchInterrupt(
		ts.lg,
		ts.stopCreationCh,
		ts.stopCreationChOnce,
		ts.osSig,
		ts.createVPC,
	); err != nil {
		return err
	}

	fmt.Printf("\n*********************************\n")
	fmt.Printf("createKeyPair (%q)\n", ts.cfg.ConfigPath)
	if err := catchInterrupt(
		ts.lg,
		ts.stopCreationCh,
		ts.stopCreationChOnce,
		ts.osSig,
		ts.createKeyPair,
	); err != nil {
		return err
	}

	fmt.Printf("\n*********************************\n")
	fmt.Printf("createASGs (%q)\n", ts.cfg.ConfigPath)
	if err := catchInterrupt(
		ts.lg,
		ts.stopCreationCh,
		ts.stopCreationChOnce,
		ts.osSig,
		ts.createASGs,
	); err != nil {
		return err
	}

	fmt.Printf("\n*********************************\n")
	fmt.Printf("createSSM (%q)\n", ts.cfg.ConfigPath)
	if err := catchInterrupt(
		ts.lg,
		ts.stopCreationCh,
		ts.stopCreationChOnce,
		ts.osSig,
		ts.createSSM,
	); err != nil {
		return err
	}

	if ts.cfg.ASGsFetchLogs {
		fmt.Printf("\n*********************************\n")
		fmt.Printf("FetchLogs (%q)\n", ts.cfg.ConfigPath)
		waitDur := 20 * time.Second
		ts.lg.Info("sleeping before FetchLogs", zap.Duration("wait", waitDur))
		time.Sleep(waitDur)
		if err := catchInterrupt(
			ts.lg,
			ts.stopCreationCh,
			ts.stopCreationChOnce,
			ts.osSig,
			ts.FetchLogs,
		); err != nil {
			return err
		}
	}

	return ts.cfg.Sync()
}

// Down cancels the cluster creation and destroy the test cluster if any.
func (ts *Tester) Down() error {
	ts.downMu.Lock()
	defer ts.downMu.Unlock()
	return ts.down()
}

func (ts *Tester) down() (err error) {
	fmt.Printf("\n*********************************\n")
	fmt.Printf("Down start (%q)\n\n", ts.cfg.ConfigPath)

	now := time.Now()
	ts.lg.Warn("starting Down",
		zap.String("name", ts.cfg.Name),
	)
	if serr := ts.uploadToS3(); serr != nil {
		ts.lg.Warn("failed to upload artifacts to S3", zap.Error(serr))
	}

	defer func() {
		ts.cfg.Sync()
		if err == nil {
			fmt.Printf("\n*********************************\n")
			fmt.Printf("Down.defer end (%q)\n\n", ts.cfg.ConfigPath)
			fmt.Printf("\n\n💯 😁 👍 :) Down success\n\n\n")

			ts.lg.Info("successfully finished Down",
				zap.String("started", humanize.RelTime(now, time.Now(), "ago", "from now")),
			)

		} else {
			fmt.Printf("\n*********************************\n")
			fmt.Printf("Down.defer end (%q)\n\n", ts.cfg.ConfigPath)
			fmt.Printf("\n\n🔥 💀 👽 😱 😡 (-_-) Down fail\n\n\n")

			ts.lg.Info("failed Down",
				zap.Error(err),
				zap.String("started", humanize.RelTime(now, time.Now(), "ago", "from now")),
			)
		}
	}()

	var errs []string

	fmt.Printf("\n*********************************\n")
	fmt.Printf("deleteSSM (%q)\n", ts.cfg.ConfigPath)
	if err := ts.deleteSSM(); err != nil {
		ts.lg.Warn("deleteSSM failed", zap.Error(err))
		errs = append(errs, err.Error())
	}

	fmt.Printf("\n*********************************\n")
	fmt.Printf("deleteASGs (%q)\n", ts.cfg.ConfigPath)
	if err := ts.deleteASGs(); err != nil {
		ts.lg.Warn("deleteASGs failed", zap.Error(err))
		errs = append(errs, err.Error())
	}

	fmt.Printf("\n*********************************\n")
	fmt.Printf("deleteKeyPair (%q)\n", ts.cfg.ConfigPath)
	if err := ts.deleteKeyPair(); err != nil {
		ts.lg.Warn("deleteKeyPair failed", zap.Error(err))
		errs = append(errs, err.Error())
	}

	fmt.Printf("\n*********************************\n")
	fmt.Printf("deleteRole (%q)\n", ts.cfg.ConfigPath)
	if err := ts.deleteRole(); err != nil {
		ts.lg.Warn("deleteRole failed", zap.Error(err))
		errs = append(errs, err.Error())
	}

	if ts.cfg.VPCCreate { // VPC was created
		waitDur := 30 * time.Second
		ts.lg.Info("sleeping before VPC deletion", zap.Duration("wait", waitDur))
		time.Sleep(waitDur)
	}

	fmt.Printf("\n*********************************\n")
	fmt.Printf("deleteVPC (%q)\n", ts.cfg.ConfigPath)
	if err := ts.deleteVPC(); err != nil {
		ts.lg.Warn("deleteVPC failed", zap.Error(err))
		errs = append(errs, err.Error())
	}

	fmt.Printf("\n*********************************\n")
	fmt.Printf("deleteS3 (%q)\n", ts.cfg.ConfigPath)
	if err := ts.deleteS3(); err != nil {
		ts.lg.Warn("deleteS3 failed", zap.Error(err))
		errs = append(errs, err.Error())
	}

	if len(errs) > 0 {
		return errors.New(strings.Join(errs, ", "))
	}
	return ts.cfg.Sync()
}
