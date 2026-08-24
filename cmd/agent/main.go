// Command agent is the per-node disk scanner. It detects raw block devices and
// publishes them as an annotation on its own Node object, which the controller
// aggregates. Intended to run as a DaemonSet.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strconv"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/jackal/rookpp/internal/disk"
)

func main() {
	var interval time.Duration
	var pathRegex string
	var minSize string
	flag.DurationVar(&interval, "interval", 60*time.Second, "scan interval")
	flag.StringVar(&pathRegex, "path-regex", "", "only report devices whose path matches this regex")
	flag.StringVar(&minSize, "min-size", "0", "minimum device size in bytes")
	flag.Parse()

	nodeName := os.Getenv("NODE_NAME")
	if nodeName == "" {
		fmt.Fprintln(os.Stderr, "NODE_NAME env var is required")
		os.Exit(1)
	}
	minBytes, _ := strconv.ParseInt(minSize, 10, 64)

	cfg, err := rest.InClusterConfig()
	if err != nil {
		fmt.Fprintln(os.Stderr, "in-cluster config:", err)
		os.Exit(1)
	}
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		fmt.Fprintln(os.Stderr, "clientset:", err)
		os.Exit(1)
	}

	filter := disk.Filter{PathRegex: pathRegex, MinSize: minBytes}
	for {
		if err := scanOnce(context.Background(), cs, nodeName, filter); err != nil {
			fmt.Fprintln(os.Stderr, "scan error:", err)
		}
		time.Sleep(interval)
	}
}

func scanOnce(ctx context.Context, cs kubernetes.Interface, nodeName string, f disk.Filter) error {
	disks, err := disk.Detect(nodeName, f)
	if err != nil {
		return fmt.Errorf("detect: %w", err)
	}
	encoded, err := disk.Encode(disks)
	if err != nil {
		return err
	}
	patch := map[string]interface{}{
		"metadata": map[string]interface{}{
			"annotations": map[string]string{disk.AnnotationKey: encoded},
		},
	}
	data, err := json.Marshal(patch)
	if err != nil {
		return err
	}
	_, err = cs.CoreV1().Nodes().Patch(ctx, nodeName, types.MergePatchType, data, metav1.PatchOptions{})
	if err != nil {
		return fmt.Errorf("patch node: %w", err)
	}
	fmt.Printf("published %d disks for node %s\n", len(disks), nodeName)
	return nil
}
