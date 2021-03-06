package workflowrun

import (
	"context"
	"fmt"
	"time"

	log "github.com/sirupsen/logrus"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/caicloud/cyclone/pkg/apis/cyclone/v1alpha1"
	"github.com/caicloud/cyclone/pkg/util"
	"github.com/caicloud/cyclone/pkg/util/k8s"
	"github.com/caicloud/cyclone/pkg/workflow/workload/delegation"
	"github.com/caicloud/cyclone/pkg/workflow/workload/pod"
)

// WorkloadProcessor processes stage workload. There are kinds of workload supported: pod, delegation.
// With pod, Cyclone would create a pod to run the stage. With delegation, Cyclone would send
// a POST request to the given URL in the workload spec.
type WorkloadProcessor struct {
	clusterClient   kubernetes.Interface
	client          k8s.Interface
	wf              *v1alpha1.Workflow
	wfr             *v1alpha1.WorkflowRun
	stg             *v1alpha1.Stage
	wfrOper         Operator
	podEventWatcher PodEventWatcher
}

// NewWorkloadProcessor ...
func NewWorkloadProcessor(clusterClient kubernetes.Interface, client k8s.Interface, wf *v1alpha1.Workflow, wfr *v1alpha1.WorkflowRun, stage *v1alpha1.Stage, wfrOperator Operator) *WorkloadProcessor {
	return &WorkloadProcessor{
		client:          client,
		clusterClient:   clusterClient,
		wf:              wf,
		wfr:             wfr,
		stg:             stage,
		wfrOper:         wfrOperator,
		podEventWatcher: newPodEventWatcher(clusterClient, client, wfr.Namespace, wfr.Name),
	}
}

// Process processes the stage according to workload type.
func (p *WorkloadProcessor) Process() error {
	if p.stg.Spec.Pod != nil && p.stg.Spec.Delegation != nil {
		return fmt.Errorf("exact 1 workload (pod or delegation) expected in stage '%s/%s', but got both", p.stg.Namespace, p.stg.Name)
	}
	if p.stg.Spec.Pod == nil && p.stg.Spec.Delegation == nil {
		return fmt.Errorf("exact 1 workload (pod or delegation) expected in stage '%s/%s', but got none", p.stg.Namespace, p.stg.Name)
	}

	if p.stg.Spec.Pod != nil {
		return p.processPod()
	}

	if p.stg.Spec.Delegation != nil {
		return p.processDelegation()
	}

	return nil
}

func (p *WorkloadProcessor) processPod() error {
	// Generate pod for this stage.
	po, err := pod.NewBuilder(p.client, p.wf, p.wfr, p.stg).Build()
	if err != nil {
		p.wfrOper.GetRecorder().Eventf(p.wfr, corev1.EventTypeWarning, "GeneratePodSpecError", "Generate pod for stage '%s' error: %v", p.stg.Name, err)
		p.wfrOper.UpdateStageStatus(p.stg.Name, &v1alpha1.Status{
			Phase:              v1alpha1.StatusFailed,
			Reason:             "GeneratePodError",
			LastTransitionTime: metav1.Time{Time: time.Now()},
			Message:            fmt.Sprintf("Failed to generate pod: %v", err),
		})
		return fmt.Errorf("create pod manifest: %w", err)
	}
	log.WithField("stg", p.stg.Name).Debug("Pod manifest created")

	po, err = p.clusterClient.CoreV1().Pods(pod.GetExecutionContext(p.wfr).Namespace).Create(context.TODO(), po, metav1.CreateOptions{})
	if err != nil {
		p.wfrOper.GetRecorder().Eventf(p.wfr, corev1.EventTypeWarning, "StagePodCreated", "Create pod for stage '%s' error: %v", p.stg.Name, err)
		var phase v1alpha1.StatusPhase
		if isExceededQuotaError(err) {
			phase = v1alpha1.StatusPending
		} else {
			phase = v1alpha1.StatusFailed
		}
		p.wfrOper.UpdateStageStatus(p.stg.Name, &v1alpha1.Status{
			Phase:              phase,
			Reason:             "CreatePodError",
			LastTransitionTime: metav1.Time{Time: time.Now()},
			Message:            fmt.Sprintf("Failed to create pod: %v", err),
		})
		return fmt.Errorf("create pod: %w", err)
	}

	log.WithField("wfr", p.wfr.Name).WithField("stg", p.stg.Name).Debug("Create pod for stage succeeded")
	p.wfrOper.GetRecorder().Eventf(p.wfr, corev1.EventTypeNormal, "StagePodCreated", "Create pod for stage '%s' succeeded", p.stg.Name)

	go p.podEventWatcher.Work(p.stg.Name, po.Namespace, po.Name)

	p.wfrOper.UpdateStageStatus(p.stg.Name, &v1alpha1.Status{
		Phase:              v1alpha1.StatusRunning,
		LastTransitionTime: metav1.Time{Time: time.Now()},
		Reason:             "StagePodCreated",
	})

	p.wfrOper.UpdateStagePodInfo(p.stg.Name, &v1alpha1.PodInfo{
		Name:      po.Name,
		Namespace: po.Namespace,
	})

	return nil
}

func (p *WorkloadProcessor) processDelegation() error {
	err := delegation.Delegate(&delegation.Request{
		Stage:       p.stg,
		Workflow:    p.wf,
		WorkflowRun: p.wfr,
	})

	if err != nil {
		p.wfrOper.GetRecorder().Eventf(p.wfr, corev1.EventTypeWarning, "DelegationFailure", "Delegate stage %s to %s error: %v", p.stg.Name, p.stg.Spec.Delegation.URL, err)
		p.wfrOper.UpdateStageStatus(p.stg.Name, &v1alpha1.Status{
			Phase:              v1alpha1.StatusFailed,
			Reason:             "DelegationFailure",
			LastTransitionTime: metav1.Time{Time: time.Now()},
			Message:            fmt.Sprintf("Delegate error: %v", err),
		})
		return err
	}

	p.wfrOper.GetRecorder().Eventf(p.wfr, corev1.EventTypeNormal, "DelegationSucceed", "Delegate stage %s to %s succeeded", p.stg.Name, p.stg.Spec.Delegation.URL)

	// If the task has already been processed by the above RESTful API task, we should not update the status of the task to Waiting
	latestWfr, err := p.client.CycloneV1alpha1().WorkflowRuns(p.wfr.Namespace).Get(context.TODO(), p.wfr.Name, metav1.GetOptions{})
	if err != nil {
		log.WithField("wfr", p.wfr.Name).Error("Get latest wfr failed")
		return err
	}
	// The task maybe has been processed by the delegation task and status not needed to be set to Waiting
	if stg, ok := latestWfr.Status.Stages[p.stg.Name]; ok {
		if util.IsPhaseTerminated(stg.Status.Phase) {
			return nil
		}
	}

	p.wfrOper.UpdateStageStatus(p.stg.Name, &v1alpha1.Status{
		Phase:              v1alpha1.StatusWaiting,
		LastTransitionTime: metav1.Time{Time: time.Now()},
		Reason:             "DelegationSucceed",
	})

	return nil
}
