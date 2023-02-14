package managesystemagent

import (
	"fmt"

	"github.com/Masterminds/semver/v3"
	"github.com/rancher/fleet/pkg/apis/fleet.cattle.io/v1alpha1"
	v3 "github.com/rancher/rancher/pkg/apis/management.cattle.io/v3"
	rancherv1 "github.com/rancher/rancher/pkg/apis/provisioning.cattle.io/v1"
	rkev1 "github.com/rancher/rancher/pkg/apis/rke.cattle.io/v1"
	"github.com/rancher/rancher/pkg/controllers/provisioningv2/rke2"
	"github.com/rancher/rancher/pkg/fleet"
	namespaces "github.com/rancher/rancher/pkg/namespace"
	"github.com/rancher/rancher/pkg/provisioningv2/image"
	"github.com/rancher/rancher/pkg/settings"
	"github.com/rancher/wrangler/pkg/name"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

func (h *handler) OnChangeInstallSUC(cluster *rancherv1.Cluster, status rancherv1.ClusterStatus) ([]runtime.Object, rancherv1.ClusterStatus, error) {
	if cluster.Spec.RKEConfig == nil {
		return nil, status, nil
	}

	currentVersion, err := semver.NewVersion(cluster.Spec.KubernetesVersion)
	if err != nil {
		return nil, status, err
	}

	// indicate to the SUC chart if we want to
	// install PodSecurityPolicy manifests
	pspEnabled := false
	if currentVersion.LessThan(Kubernetes125) {
		pspEnabled = true
	}

	// we must limit the output of name.SafeConcatName to at most 48 characters because
	// a) the chart release name cannot exceed 53 characters, and
	// b) upon creation of this resource the prefix 'mcc-' will be added to the release name, hence the limiting to 48 characters
	managedChartName := name.Limit(name.SafeConcatName(cluster.Name, "managed", "system-upgrade-controller"), 48)

	mcc := &v3.ManagedChart{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: cluster.Namespace,
			Name:      managedChartName,
		},
		Spec: v3.ManagedChartSpec{
			DefaultNamespace: namespaces.System,
			RepoName:         "rancher-charts",
			Chart:            "system-upgrade-controller",
			Version:          settings.SystemUpgradeControllerChartVersion.Get(),
			Values: &v1alpha1.GenericMap{
				Data: map[string]interface{}{
					"global": map[string]interface{}{
						"cattle": map[string]interface{}{
							"systemDefaultRegistry": image.GetPrivateRepoURLFromCluster(cluster),
							"psp": map[string]interface{}{
								"enabled": pspEnabled,
							},
						},
					},
				},
			},
			Targets: []v1alpha1.BundleTarget{
				{
					ClusterName: cluster.Name,
					ClusterSelector: &metav1.LabelSelector{
						MatchExpressions: []metav1.LabelSelectorRequirement{
							{
								Key:      "provisioning.cattle.io/unmanaged-system-agent",
								Operator: metav1.LabelSelectorOpDoesNotExist,
							},
						},
					},
				},
			},
		},
	}

	return []runtime.Object{
		mcc,
	}, status, nil
}

// syncSystemUpgradeControllerStatus queries the managed system-upgrade-controller chart and determines if it is properly configured for a given
// version of Kubernetes. It applies a condition onto the control-plane object to be used by the planner when handling Kubernetes upgrades.
func (h *handler) syncSystemUpgradeControllerStatus(obj *rkev1.RKEControlPlane, status rkev1.RKEControlPlaneStatus) (rkev1.RKEControlPlaneStatus, error) {
	// perform the same name limiting as in the OnChangeInstallSUC controller, but prepend the 'mcc-' prefix that is added when the bundle is created
	bundleName := fmt.Sprintf("mcc-%s", name.Limit(name.SafeConcatName(obj.Name, "managed", "system-upgrade-controller"), 48))
	sucBundle, err := h.bundles.Get(fleet.ClustersDefaultNamespace, bundleName, metav1.GetOptions{})
	if errors.IsNotFound(err) {
		// if we couldn't find the bundle then we know it's not ready
		rke2.SystemUpgradeControllerReady.False(&status)
		// don't return the error, otherwise the status won't be set to 'false'
		return status, nil
	}

	if err != nil {
		return status, err
	}

	rke2.SystemUpgradeControllerReady.Unknown(&status)

	// determine if the SUC deployment has been rolled out fully, and if there were any errors encountered
	if sucBundle.Status.Summary.Ready != sucBundle.Status.Summary.DesiredReady {
		if sucBundle.Status.Summary.ErrApplied != 0 && len(sucBundle.Status.Summary.NonReadyResources) > 0 {
			nonReady := sucBundle.Status.Summary.NonReadyResources
			rke2.SystemUpgradeControllerReady.Reason(&status, fmt.Sprintf("Error Encountered Waiting for System Upgrade Controller Deployment To Roll Out: %s", nonReady[0].Message))
			return status, nil
		}
		rke2.SystemUpgradeControllerReady.Reason(&status, "Waiting for System Upgrade Controller Deployment roll out")
		return status, nil
	}

	// we need to look at the values yaml content to determine if PSPs are enabled
	// or disabled. We need to wait until SUC is redeployed if we don't see any helm values, as
	// we expect the PSP value to be explicitly defined as either true or false
	valuesYamlAvailable := sucBundle.Spec.Helm != nil && sucBundle.Spec.Helm.Values != nil
	if !valuesYamlAvailable {
		rke2.SystemUpgradeControllerReady.Reason(&status, "Waiting for Upgraded System Upgrade Controller Deployment")
		return status, nil
	}

	// look through the values yaml content to determine if 'psp: enabled: true'
	rke2.SystemUpgradeControllerReady.Reason(&status, "Waiting for System Upgrade Controller Bundle Update")
	data := sucBundle.Spec.Helm.Values.Data
	global, ok := data["global"].(map[string]interface{})
	if !ok {
		return status, nil
	}

	cattle, ok := global["cattle"].(map[string]interface{})
	if !ok {
		return status, nil
	}

	psp, ok := cattle["psp"].(map[string]interface{})
	if !ok {
		return status, nil
	}

	currentVersion, err := semver.NewVersion(obj.Spec.KubernetesVersion)
	if err != nil {
		return status, err
	}

	// we only want to block an upgrade if PSPs are enabled AND we are on
	// a version greater than or equal to 1.25.
	enabled, ok := psp["enabled"].(bool)
	if !ok {
		return status, nil
	}

	if !currentVersion.LessThan(Kubernetes125) && enabled {
		rke2.SystemUpgradeControllerReady.Reason(&status, "System Upgrade Controller Not Ready")
		return status, nil
	}

	rke2.SystemUpgradeControllerReady.True(&status)
	return status, nil
}
