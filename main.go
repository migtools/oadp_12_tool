package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"path/filepath"
	"time"

	"github.com/google/uuid"
	v1 "github.com/kubernetes-csi/external-snapshotter/client/v4/apis/volumesnapshot/v1"
	"github.com/pkg/errors"
	velerov1 "github.com/vmware-tanzu/velero/pkg/apis/velero/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/homedir"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var namespaces = []string{
	"mysql-persistent",
}

func main() {
	ctx := context.Background()
	// Build client from default kubeconfig or --kubeconfig flag
	var kubeconfig *string
	if home := homedir.HomeDir(); home != "" {
		kubeconfig = flag.String("kubeconfig", filepath.Join(home, ".kube", "config"), "(optional) absolute path to the kubeconfig file")
	} else {
		kubeconfig = flag.String("kubeconfig", "", "absolute path to the kubeconfig file")
	}
	flag.Parse()

	// use the current context in kubeconfig
	config, err := clientcmd.BuildConfigFromFlags("", *kubeconfig)
	if err != nil {
		panic(err.Error())
	}
	scheme := runtime.NewScheme()
	velerov1.AddToScheme(scheme)
	v1.AddToScheme(scheme)
	c, err := client.New(config, client.Options{Scheme: scheme})
	if err != nil {
		panic(err.Error())
	}
	// create backup to get all CSI snapshots in the cluster
	name, err := createBackup(ctx, c, namespaces)
	if err != nil {
		panic(err.Error())
	}
	log.Printf("backup created openshift-adp/%s. To monitor VSCs run:", name)
	log.Printf("oc get volumesnapshotcontents -l velero.io/backup-name=%s", name)

	// Sit and wait for all VSCs to be in a ready to use state
	err = waitForVSCsToBeReady(ctx, c, name)
	if err != nil {
		if err == wait.ErrWaitTimeout {
			log.Printf("Timed out waiting for VSCs to be ready")
		}
		panic(err.Error())
	}

	// Now that VSCs are all ready, we can generate VolumeSnapshotBackups
	// and batch them waiting for them to complete
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

func listVolumeSnapshotContents(ctx context.Context, c client.Client, name string) (*v1.VolumeSnapshotContentList, error) {
	vsc := v1.VolumeSnapshotContentList{}
	labels := map[string]string{
		"velero.io/backup-name": name,
	}
	listOptions := client.MatchingLabels(labels)
	err := c.List(ctx, &vsc, listOptions)
	return &vsc, err
}

func createBackup(ctx context.Context, c client.Client, namespaces []string) (string, error) {
	name := uuid.New()
	b := velerov1.Backup{}
	b.Spec.IncludedNamespaces = namespaces
	b.Namespace = "openshift-adp"
	b.Name = name.String()
	return name.String(), c.Create(ctx, &b)
}
