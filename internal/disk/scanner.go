// Package disk implements node-local raw block device detection. It is intended
// to run as a DaemonSet agent; results are published as a node annotation that
// the controller aggregates.
package disk

import (
	"encoding/json"
	"os/exec"
	"regexp"
	"strings"

	storagev1alpha1 "github.com/jackal/rookpp/api/v1alpha1"
)

// AnnotationKey is the node annotation where the agent publishes detected disks.
const AnnotationKey = "storage.jackal.io/detected-disks"

// Filter constrains which devices are reported.
type Filter struct {
	PathRegex string
	MinSize   int64 // bytes; 0 = no minimum
}

// lsblkDevice mirrors the fields we need from `lsblk -J -b -O`.
type lsblkDevice struct {
	Name       string        `json:"name"`
	Path       string        `json:"path"`
	Size       int64         `json:"size"`
	Type       string        `json:"type"`
	Rota       bool          `json:"rota"`
	Mountpoint string        `json:"mountpoint"`
	FSType     string        `json:"fstype"`
	Model      string        `json:"model"`
	PkName     string        `json:"pkname"`
	Children   []lsblkDevice `json:"children"`
}

type lsblkOutput struct {
	BlockDevices []lsblkDevice `json:"blockdevices"`
}

// runLsblk is overridable for testing.
var runLsblk = func() ([]byte, error) {
	return exec.Command("lsblk", "-J", "-b", "-O").Output()
}

// Detect returns the empty/raw disks on the local node matching the filter.
func Detect(nodeName string, f Filter) ([]storagev1alpha1.DetectedDisk, error) {
	out, err := runLsblk()
	if err != nil {
		return nil, err
	}
	return parseAndFilter(out, nodeName, f)
}

func parseAndFilter(out []byte, nodeName string, f Filter) ([]storagev1alpha1.DetectedDisk, error) {
	var parsed lsblkOutput
	if err := json.Unmarshal(out, &parsed); err != nil {
		return nil, err
	}
	var re *regexp.Regexp
	if f.PathRegex != "" {
		var err error
		re, err = regexp.Compile(f.PathRegex)
		if err != nil {
			return nil, err
		}
	}

	var disks []storagev1alpha1.DetectedDisk
	for i := range parsed.BlockDevices {
		d := &parsed.BlockDevices[i]
		if !isEmptyDisk(d) {
			continue
		}
		if f.MinSize > 0 && d.Size < f.MinSize {
			continue
		}
		path := d.Path
		if path == "" {
			path = "/dev/" + d.Name
		}
		if re != nil && !re.MatchString(path) {
			continue
		}
		disks = append(disks, storagev1alpha1.DetectedDisk{
			Node:       nodeName,
			Path:       path,
			SizeBytes:  d.Size,
			Rotational: d.Rota,
			Model:      strings.TrimSpace(d.Model),
		})
	}
	return disks, nil
}

// isEmptyDisk reports whether a device is a whole disk with no filesystem,
// no mountpoint, and no children (partitions / LVM / mapper users).
func isEmptyDisk(d *lsblkDevice) bool {
	if d.Type != "disk" {
		return false
	}
	if len(d.Children) > 0 {
		return false
	}
	if d.Mountpoint != "" {
		return false
	}
	if d.FSType != "" {
		return false
	}
	return true
}

// Encode marshals detected disks for storage in a node annotation.
func Encode(disks []storagev1alpha1.DetectedDisk) (string, error) {
	b, err := json.Marshal(disks)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// Decode parses a node annotation value into detected disks.
func Decode(s string) ([]storagev1alpha1.DetectedDisk, error) {
	if strings.TrimSpace(s) == "" {
		return nil, nil
	}
	var disks []storagev1alpha1.DetectedDisk
	if err := json.Unmarshal([]byte(s), &disks); err != nil {
		return nil, err
	}
	return disks, nil
}
