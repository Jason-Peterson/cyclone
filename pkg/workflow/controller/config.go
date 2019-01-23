package controller

import (
	"encoding/json"
	"fmt"
	"os"
	"time"

	log "github.com/sirupsen/logrus"
	corev1 "k8s.io/api/core/v1"

	"github.com/caicloud/cyclone/pkg/apis/cyclone/v1alpha1"
)

const (
	// DevModeEnvName determines whether workflow controller is in development mode.
	// In development mode, resource resolver containers, coordinator containers will
	// have image pull policy being 'Always', otherwise it's 'IfNotPresent'.
	DevModeEnvName = "DEVELOP_MODE"

	// ConfigFileKey is key of config file in ConfigMap
	ConfigFileKey = "workflow-controller.json"

	// GitResolverImage is key of git source resolver image in config file
	GitResolverImage = "git-resolver"
	// ImageResolverImage is key of image source resolver image in config file
	ImageResolverImage = "image-resolver"
	// KvResolverImage is key of kv source resolver image in config file
	KvResolverImage = "kv-resolver"
	// CoordinatorImage is key of coordinator image in config file
	CoordinatorImage = "coordinator"
	// GCImage is key of the GC image in config file
	GCImage = "gc"
)

// ResolverImageKeys maps resource type to resolver images
var ResolverImageKeys = map[v1alpha1.ResourceType]string{
	v1alpha1.GitResourceType:   GitResolverImage,
	v1alpha1.ImageResourceType: ImageResolverImage,
	v1alpha1.KVResourceType:    KvResolverImage,
}

// WorkflowControllerConfig configures Workflow Controller
type WorkflowControllerConfig struct {
	// Images that used in controller, such as resource resolvers.
	Images map[string]string `json:"images"`
	// Logging configuration, such as log level.
	Logging LoggingConfig `json:"logging"`
	// GC configuration
	GC GCConfig `json:"gc"`
	// Limits of each resources should be retained
	Limits LimitsConfig `json:"limits"`
	// ResourceRequirements is default resource requirements for containers in stage Pod
	ResourceRequirements corev1.ResourceRequirements `json:"default_resource_quota"`
	// PVC used to transfer artifacts in WorkflowRun, and also to help share resources
	// among stages within WorkflowRun. If no PVC is given here, input resources won't be
	// shared among stages, but need to be pulled every time it's needed. And also if no
	// PVC given, artifacts are not supported.
	// TODO(ChenDe): Remove it when Cyclone can manage PVC for namespaces.
	PVC string `json:"pvc"`
	// Secret is default secret used for Cyclone, auth of registry can be placed here. It's optional.
	// TODO(ChenDe): Remove it when Cyclone can manage secrets for namespaces.
	Secret string `json:"secret"`
	// CycloneServerAddr is address of the Cyclone Server
	CycloneServerAddr string `json:"cyclone_server_addr"`
}

// LoggingConfig configures logging
type LoggingConfig struct {
	Level string `json:"level"`
}

// GCConfig configures GC
type GCConfig struct {
	// Enabled controllers whether GC is enabled, if set to false, no GC would happen.
	Enabled bool `json:"enabled"`
	// DelaySeconds defines the time after a WorkflowRun terminated to perform GC. When configured to 0.
	// it equals to gc immediately.
	DelaySeconds time.Duration `json:"delay_seconds"`
	// RetryCount defines how many times to retry when GC failed, 0 means no retry.
	RetryCount int `json:"retry"`
}

// LimitsConfig configures maximum WorkflowRun to keep for each Workflow
type LimitsConfig struct {
	// Maximum WorkflowRuns to be kept for each Workflow
	MaxWorkflowRuns int `json:"max_workflowruns"`
}

// Config is Workflow Controller config instance
var Config WorkflowControllerConfig

// LoadConfig loads configuration from ConfigMap
func LoadConfig(cm *corev1.ConfigMap) error {
	data, ok := cm.Data[ConfigFileKey]
	if !ok {
		return fmt.Errorf("ConfigMap '%s' doesn't have data key '%s'", cm.Name, ConfigFileKey)
	}
	err := json.Unmarshal([]byte(data), &Config)
	if err != nil {
		log.WithField("data", data).Debug("Unmarshal config data error: ", err)
		return err
	}

	if !validate(&Config) {
		return fmt.Errorf("validate config failed")
	}

	InitLogger(&Config.Logging)
	return nil
}

// validate validates some required configurations.
func validate(config *WorkflowControllerConfig) bool {
	if config.PVC == "" {
		log.Warn("PVC not configured, resources won't be shared among stages and artifacts unsupported.")
	}

	if config.Secret == "" {
		log.Warn("Secret not configured, no auth information would be available, e.g. docker registry auth.")
	}

	for _, k := range []string{GitResolverImage, ImageResolverImage, KvResolverImage, CoordinatorImage} {
		_, ok := config.Images[k]
		if !ok {
			return false
		}
	}

	return true
}

// ImagePullPolicy determines image pull policy based on environment variable DEVELOP_MODE
// This pull policy will be used in image resolver containers and coordinator containers.
func ImagePullPolicy() corev1.PullPolicy {
	if os.Getenv(DevModeEnvName) == "true" {
		return corev1.PullAlways
	}
	return corev1.PullIfNotPresent
}
