package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
	dmv1 "github.com/konveyor/volume-snapshot-mover/api/v1alpha1"
	v1 "github.com/kubernetes-csi/external-snapshotter/client/v4/apis/volumesnapshot/v1"
	"github.com/pkg/errors"
	velerov1 "github.com/vmware-tanzu/velero/pkg/apis/velero/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/homedir"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func main() {
	resticSecretName := flag.String("restic-secret", "dpa-sample-1-volsync-restic", "name of restic secret for volsync to use")
	namespacesInput := flag.String("namespaces", "", "comma separated list of namespaces to backup")
	concurrentInput := flag.Int("concurrent", 12, "number of concurrent volumesnapshotbackups to run")
	labelSelectorsInput := flag.String("selectors", "", "comma seperated list of labels to be used as criteria for backup")
	ctx := context.Background()
	// Build client from default kubeconfig or --kubeconfig flag
	var kubeconfig *string
	if home := homedir.HomeDir(); home != "" {
		kubeconfig = flag.String("kubeconfig", filepath.Join(home, ".kube", "config"), "(optional) absolute path to the kubeconfig file")
	} else {
		kubeconfig = flag.String("kubeconfig", "", "absolute path to the kubeconfig file")
	}
	flag.Parse()

	namespaces := strings.Split(*namespacesInput, ",")
	if *namespacesInput == "" {
		panic(errors.New("missing namespaces flag"))
	}
	var labelSelectorsMap = make(map[string]string)
	if len(*labelSelectorsInput) > 0 {
		labelSelectors := strings.Split(*labelSelectorsInput, ",")
		for _, l := range labelSelectors {
			splitStr := strings.Split(l, "=")
			key := splitStr[0]
			value := splitStr[1]
			labelSelectorsMap[key] = value
		}
	}

	// use the current context in kubeconfig
	config, err := clientcmd.BuildConfigFromFlags("", *kubeconfig)
	if err != nil {
		panic(err.Error())
	}
	scheme := runtime.NewScheme()
	velerov1.AddToScheme(scheme)
	v1.AddToScheme(scheme)
	dmv1.AddToScheme(scheme)
	c, err := client.New(config, client.Options{Scheme: scheme})
	if err != nil {
		panic(err.Error())
	}

	// Register start time for snapshots
	snapshotStartTime := time.Now()

	// create backup to get all CSI snapshots in the cluster
	name, err := createBackup(ctx, c, namespaces, labelSelectorsMap)
	if err != nil {
		panic(err.Error())
	}
	log.Printf("backup created openshift-adp/%s. To monitor VSCs run:", name)
	log.Printf("oc get volumesnapshotcontents -l velero.io/backup-name=%s", name)

	// Wait for backup to complete
	err = waitForBackupToComplete(ctx, c, name)
	if err != nil {
		if err == wait.ErrWaitTimeout {
			log.Printf("Timed out waiting for Backup to complete")
		}
		panic(err.Error())
	}

	// Sit and wait for all VSCs to be in a ready to use state
	err = waitForVSCsToBeReady(ctx, c, name)
	if err != nil {
		if err == wait.ErrWaitTimeout {
			log.Printf("Timed out waiting for VSCs to be ready")
		}
		panic(err.Error())
	}

	snapshotEndTime := time.Now()
	snapshotTime := snapshotEndTime.Sub(snapshotStartTime)
	log.Printf("Snapshot time elapsed: %v", snapshotTime.String())

	// Now that VSCs are all ready, we can generate VolumeSnapshotBackups
	// and batch them waiting for them to complete
	vscList, err := listVolumeSnapshotContents(ctx, c, name)
	if err != nil {
		panic(err)
	}
	// create 12 VSBs at a time
	for i := 0; i < len(vscList.Items); i += *concurrentInput {
		var section []v1.VolumeSnapshotContent
		if i > len(vscList.Items)-*concurrentInput {
			section = vscList.Items[i:]
		} else {
			section = vscList.Items[i : i+*concurrentInput]
		}
		log.Printf("Processing %v volumesnapshotcontents", len(section))
		for _, vsc := range section {
			vsb := dmv1.VolumeSnapshotBackup{
				ObjectMeta: metav1.ObjectMeta{
					GenerateName: "vsb-",
					Namespace:    vsc.Spec.VolumeSnapshotRef.Namespace,
					Labels: map[string]string{
						"perf-test":             name,
						"velero.io/backup-name": name,
					},
				},

				Spec: dmv1.VolumeSnapshotBackupSpec{
					VolumeSnapshotContent: corev1.ObjectReference{
						Name: vsc.Name,
					},
					ProtectedNamespace: "openshift-adp",
					ResticSecretRef: corev1.LocalObjectReference{
						Name: *resticSecretName,
					},
				},
			}
			err := c.Create(ctx, &vsb)
			if err != nil {
				log.Printf("ERROR creating VSB for vsc %s; %v", vsc.Name, err.Error())
			}

		}
		// wait for VSBs to be complete

		err = waitForVSBsToComplete(ctx, c, name)
		if err != nil {
			if err == wait.ErrWaitTimeout {
				log.Printf("Timed out waiting for VSBs to be ready")
			}
			panic(err.Error())
		}
	}

	volsyncTimeComplete := time.Now()
	volsyncTime := volsyncTimeComplete.Sub(snapshotEndTime)
	totalTime := volsyncTimeComplete.Sub(snapshotStartTime)
	log.Printf("Data Mover time elapsed: %v", volsyncTime.String())
	log.Printf("Total time: %v", totalTime.String())
}

func waitForBackupToComplete(ctx context.Context, c client.Client, name string) error {
	timeout := 120 * time.Minute
	interval := 5 * time.Second
	err := wait.PollImmediate(interval, timeout, func() (bool, error) {
		backup := velerov1.Backup{}
		err := c.Get(ctx, types.NamespacedName{Name: name, Namespace: "openshift-adp"}, &backup)
		if err != nil {
			return false, errors.Wrapf(err, fmt.Sprintf("failed to get backup"))
		}
		if backup.Status.Phase == velerov1.BackupPhaseCompleted {
			return true, nil
		}
		log.Printf("Backup phase: %v", backup.Status.Phase)

		return false, nil
	})
	return err
}

func waitForVSCsToBeReady(ctx context.Context, c client.Client, name string) error {
	timeout := 120 * time.Minute
	interval := 5 * time.Second
	err := wait.PollImmediate(interval, timeout, func() (bool, error) {
		vscList, err := listVolumeSnapshotContents(ctx, c, name)
		if err != nil {
			return false, errors.Wrapf(err, fmt.Sprintf("failed to list volumesnapshotcontents %s", err.Error()))
		}
		if len(vscList.Items) == 0 {
			log.Printf("found no snapshots yet, waiting...")
			return false, nil

		}
		log.Printf("found %v total snapshots", len(vscList.Items))
		readyVscs := []string{}
		unreadyVscs := []string{}
		for _, vsc := range vscList.Items {
			if vsc.Status == nil || vsc.Status.SnapshotHandle == nil || *vsc.Status.ReadyToUse != true {
				unreadyVscs = append(unreadyVscs, vsc.Name)
				continue
			}
			readyVscs = append(readyVscs, vsc.Name)
		}
		log.Printf("found %v ready VSCs, and %v unready VSCs", len(readyVscs), len(unreadyVscs))

		if len(unreadyVscs) != 0 {
			return false, nil
		}

		return true, nil
	})
	return err
}

func waitForVSBsToComplete(ctx context.Context, c client.Client, name string) error {
	timeout := 120 * time.Minute
	interval := 5 * time.Second
	err := wait.PollImmediate(interval, timeout, func() (bool, error) {
		vscList, err := listVolumeSnapshotBackups(ctx, c, name)
		if err != nil {
			return false, errors.Wrapf(err, fmt.Sprintf("failed to list volumesnapshotcontents %s", err.Error()))
		}
		if len(vscList.Items) == 0 {
			log.Printf("found no snapshots yet, waiting...")
			return false, nil

		}
		readyVscs := []string{}
		running := []string{}
		for _, vsc := range vscList.Items {
			if vsc.Status.Phase == dmv1.SnapMoverVolSyncPhaseCompleted || vsc.Status.Phase == dmv1.SnapMoverBackupPhaseCompleted {
				readyVscs = append(readyVscs, vsc.Name)
			} else {
				running = append(running, vsc.Name)
			}
		}
		log.Printf("found %v completed VSBs, and %v running VSBs", len(readyVscs), len(running))

		if len(running) != 0 {
			return false, nil
		}

		return true, nil
	})
	return err
}

func listVolumeSnapshotContents(ctx context.Context, c client.Client, name string) (*v1.VolumeSnapshotContentList, error) {
	vsc := v1.VolumeSnapshotContentList{}
	labels := map[string]string{
		"velero.io/backup-name": name,
	}
	listOptions := client.MatchingLabels(labels)
	err := c.List(ctx, &vsc, listOptions)
	return &vsc, err
}

func listVolumeSnapshotBackups(ctx context.Context, c client.Client, name string) (*dmv1.VolumeSnapshotBackupList, error) {
	vsb := dmv1.VolumeSnapshotBackupList{}
	labels := map[string]string{
		"perf-test": name,
	}
	listOptions := client.MatchingLabels(labels)
	err := c.List(ctx, &vsb, listOptions)
	return &vsb, err
}

func createBackup(ctx context.Context, c client.Client, namespaces []string, labelSelectorsMap map[string]string) (string, error) {
	name := uuid.New()
	b := velerov1.Backup{}
	b.Spec.IncludedNamespaces = namespaces

	if labelSelectorsMap != nil {
		labels := metav1.LabelSelector{
			MatchLabels: labelSelectorsMap,
		}

		b.Spec.LabelSelector = &labels
	}

	b.Namespace = "openshift-adp"
	b.Name = name.String()
	return name.String(), c.Create(ctx, &b)
}
