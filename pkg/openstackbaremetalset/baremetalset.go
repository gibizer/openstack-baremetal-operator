package openstackbaremetalset

import (
	"context"
	"fmt"
	"strings"

	metal3v1alpha1 "github.com/metal3-io/baremetal-operator/apis/metal3.io/v1alpha1"
	"github.com/openstack-k8s-operators/lib-common/modules/common/helper"
	baremetalv1 "github.com/openstack-k8s-operators/openstack-baremetal-operator/api/v1beta1"
	corev1 "k8s.io/api/core/v1"
	k8s_errors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// VerifyBaremetalStatusBmhRefs - Verify that BMHs haven't been improperly deleted
// outside of our prescribed annotate-and-scale-count-down method.  If bad deletions
// have occurred, we return an error to halt further reconciliation that could lead
// to an inconsistent state for instance.Status.BaremetalHosts.
func VerifyBaremetalStatusBmhRefs(
	ctx context.Context,
	helper *helper.Helper,
	instance *baremetalv1.OpenStackBaremetalSet,
) error {
	// Get all BaremetalHosts
	allBaremetalHosts, err := GetBaremetalHosts(
		ctx,
		helper,
		"openshift-machine-api",
		map[string]string{},
	)
	if err != nil {
		return err
	}

	for _, bmhStatus := range instance.Status.BaremetalHosts {
		found := false

		for _, bmh := range allBaremetalHosts.Items {
			if bmh.Name == bmhStatus.BmhRef {
				found = true
				break
			}
		}

		if !found {
			err := fmt.Errorf("Existing BaremetalHost \"%s\" not found for OsBaremetalSet %s.  "+
				"Please check BaremetalHost resources and re-add \"%s\" to continue",
				bmhStatus.BmhRef, instance.Name, bmhStatus.BmhRef)

			return err
		}
	}

	return nil
}

// VerifyBaremetalSetScaleUp -
func VerifyBaremetalSetScaleUp(
	ctx context.Context,
	helper *helper.Helper,
	instance *baremetalv1.OpenStackBaremetalSet,
	allBmhs *metal3v1alpha1.BareMetalHostList,
	existingBmhs *metal3v1alpha1.BareMetalHostList) ([]string, error) {
	l := log.FromContext(ctx)

	// How many new BaremetalHost allocations do we need (if any)?
	newBmhsNeededCount := len(instance.Spec.BaremetalHosts) - len(existingBmhs.Items)
	availableBaremetalHosts := []string{}

	if newBmhsNeededCount > 0 {
		// We have new BaremetalHosts requested, so search for BaremetalHosts that don't have consumerRef or online set

		for _, baremetalHost := range allBmhs.Items {
			if baremetalHost.Spec.Online || baremetalHost.Spec.ConsumerRef != nil {
				continue
			}

			if !verifyBaremetalSetHardwareMatch(ctx, helper, instance, &baremetalHost) {
				l.Info("BaremetalHost does not match hardware requirements", "BMH", baremetalHost.ObjectMeta.Name)
				continue
			}

			l.Info("Available BaremetalHost", "BMH", baremetalHost.ObjectMeta.Name)

			availableBaremetalHosts = append(availableBaremetalHosts, baremetalHost.ObjectMeta.Name)
		}

		// If we can't satisfy the new requested BaremetalHost count, explicitly state so
		if newBmhsNeededCount > len(availableBaremetalHosts) {
			return nil, fmt.Errorf("Unable to find %d requested BaremetalHosts for scale-up (%d in use, %d available)",
				len(instance.Spec.BaremetalHosts),
				len(existingBmhs.Items),
				len(availableBaremetalHosts))
		}

	}

	return availableBaremetalHosts, nil
}

// VerifyBaremetalSetScaleDown - TODO: not needed at the current moment
func VerifyBaremetalSetScaleDown(
	ctx context.Context,
	helper *helper.Helper, instance *baremetalv1.OpenStackBaremetalSet,
	existingBmhs *metal3v1alpha1.BareMetalHostList,
	removalAnnotatedBmhCount int) error {
	// How many new BaremetalHost de-allocations do we need (if any)?
	bmhsToRemoveCount := len(existingBmhs.Items) - len(instance.Spec.BaremetalHosts)

	if bmhsToRemoveCount > removalAnnotatedBmhCount {
		return fmt.Errorf("Unable to find sufficient amount of BaremetalHosts annotated for scale-down (%d found, %d requested)",
			removalAnnotatedBmhCount,
			bmhsToRemoveCount)
	}

	return nil
}

func verifyBaremetalSetHardwareMatch(
	ctx context.Context,
	helper *helper.Helper,
	instance *baremetalv1.OpenStackBaremetalSet,
	bmh *metal3v1alpha1.BareMetalHost,
) bool {
	l := log.FromContext(ctx)

	// If no requested hardware requirements, we're all set
	if instance.Spec.HardwareReqs == (baremetalv1.HardwareReqs{}) {
		return true
	}

	// Can't make comparisons if the BMH lacks hardware details
	if bmh.Status.HardwareDetails == nil {
		l.Info("WARNING: BaremetalHost lacks hardware details in status; cannot verify against hardware requests!", "BMH", bmh.Name)
		return false
	}

	cpuReqs := instance.Spec.HardwareReqs.CPUReqs

	// CPU architecture is always exact-match only
	if cpuReqs.Arch != "" && bmh.Status.HardwareDetails.CPU.Arch != cpuReqs.Arch {
		l.Info("BaremetalHost CPU arch does not match request",
			"BMH",
			bmh.Name,
			"CPU arch",
			bmh.Status.HardwareDetails.CPU.Arch,
			"CPU arch request",
			cpuReqs.Arch)

		return false
	}

	// CPU count can be exact-match or (default) greater
	if cpuReqs.CountReq.Count != 0 && bmh.Status.HardwareDetails.CPU.Count != cpuReqs.CountReq.Count {
		if cpuReqs.CountReq.ExactMatch || cpuReqs.CountReq.Count > bmh.Status.HardwareDetails.CPU.Count {
			l.Info("BaremetalHost CPU count does not match request",
				"BMH",
				bmh.Name,
				"CPU count",
				bmh.Status.HardwareDetails.CPU.Count,
				"CPU count request",
				cpuReqs.CountReq.Count)

			return false
		}
	}

	// CPU clock speed can be exact-match or (default) greater
	if cpuReqs.MhzReq.Mhz != 0 {
		clockSpeed := int(bmh.Status.HardwareDetails.CPU.ClockMegahertz)
		if cpuReqs.MhzReq.Mhz != clockSpeed && (cpuReqs.MhzReq.ExactMatch || cpuReqs.MhzReq.Mhz > clockSpeed) {
			l.Info("BaremetalHost CPU mhz does not match request",
				"BMH",
				bmh.Name,
				"CPU mhz",
				clockSpeed,
				"CPU mhz request",
				cpuReqs.MhzReq.Mhz)

			return false
		}
	}

	memReqs := instance.Spec.HardwareReqs.MemReqs

	// Memory GBs can be exact-match or (default) greater
	if memReqs.GbReq.Gb != 0 {
		memGbBms := float64(memReqs.GbReq.Gb)
		memGbBmh := float64(bmh.Status.HardwareDetails.RAMMebibytes) / float64(1024)

		if memGbBmh != memGbBms && (memReqs.GbReq.ExactMatch || memGbBms > memGbBmh) {
			l.Info("BaremetalHost memory size does not match request",
				"BMH",
				bmh.Name,
				"Memory size",
				memGbBmh,
				"Memory size request",
				memGbBms)

			return false
		}
	}

	diskReqs := instance.Spec.HardwareReqs.DiskReqs

	var foundDisk *metal3v1alpha1.Storage

	if diskReqs.GbReq.Gb != 0 {
		diskGbBms := float64(diskReqs.GbReq.Gb)
		// TODO: Make sure there's at least one disk of this size?
		for _, disk := range bmh.Status.HardwareDetails.Storage {
			diskGbBmh := float64(disk.SizeBytes) / float64(1073741824)

			if diskGbBmh == diskGbBms || (!diskReqs.GbReq.ExactMatch && diskGbBmh > diskGbBms) {
				foundDisk = &disk
				break
			}
		}

		if foundDisk == nil {
			l.Info("BaremetalHost does not contain a disk of proper size that matches request",
				"BMH",
				bmh.Name,
				"Disk size request",
				diskGbBms)

			return false
		}
	}

	// We only care about the SSD flag if the user requested an exact match for it or if SSD is true
	if diskReqs.SSDReq.ExactMatch || diskReqs.SSDReq.SSD {
		found := false

		// If we matched on a disk for a GbReqs above, we need to match on the same disk
		if foundDisk != nil {
			if foundDisk.Rotational != diskReqs.SSDReq.SSD {
				found = true
			}
		} else {
			// TODO: Just need to match on any disk?
			for _, disk := range bmh.Status.HardwareDetails.Storage {
				if disk.Rotational != diskReqs.SSDReq.SSD {
					found = true
				}
			}
		}

		if !found {
			l.Info("BaremetalHost does not contain a disk that matches 'is rotational' request",
				"BMH",
				bmh.Name,
				"Rotational disk wanted",
				diskReqs.SSDReq.SSD)

			return false
		}
	}

	l.Info("BaremetalHost satisfies hardware requirements", "BMH", bmh.Name)

	return true
}

// GetDeletionAnnotatedBaremetalHosts - Returns all BMHs in the namespace that are annotated for deletion
func GetDeletionAnnotatedBaremetalHosts(
	ctx context.Context,
	helper *helper.Helper,
	namespace string,
	labelSelector map[string]string,
) ([]string, error) {
	baremetalHostList, err := GetBaremetalHosts(ctx, helper, namespace, labelSelector)
	if err != nil {
		return []string{}, err
	}

	return getDeletionAnnotatedBaremetalHosts(baremetalHostList), nil
}

func getDeletionAnnotatedBaremetalHosts(
	baremetalHostList *metal3v1alpha1.BareMetalHostList,
) []string {
	annotatedBMHs := []string{}

	// Find deletion annotated BaremetalHosts belonging to this OpenStackBaremetalSet
	for _, baremetalHost := range baremetalHostList.Items {

		// Get list of OSP hostnames from HostRemovalAnnotation annotated BMHs
		if val, ok := baremetalHost.Annotations[HostRemovalAnnotation]; ok && (strings.ToLower(val) == "yes" || strings.ToLower(val) == "true") {
			annotatedBMHs = append(annotatedBMHs, baremetalHost.GetName())
		}
	}

	return annotatedBMHs
}

func GetBmhRefStatus(
	instance *baremetalv1.OpenStackBaremetalSet,
	bmh string,
) (baremetalv1.HostStatus, error) {

	for _, bmhStatus := range instance.Status.DeepCopy().BaremetalHosts {
		if bmhStatus.BmhRef == bmh {
			return bmhStatus, nil
		}
	}

	return baremetalv1.HostStatus{}, k8s_errors.NewNotFound(corev1.Resource("OpenStackBaremetalHostStatus"), "not found")
}
