// Package discovery inventories whole disks without modifying devices.
package discovery

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/SongOf/pulsar-operator/internal/agent/host"
	"github.com/SongOf/pulsar-operator/internal/agent/model"
)

var (
	partitionAlias = regexp.MustCompile(`-part[0-9]+$`)
	deviceNumber   = regexp.MustCompile(`^[0-9]+:[0-9]+$`)
)

// Discoverer permits fixture-backed discovery; zero fields select host defaults.
type Discoverer struct {
	Runner                  host.Runner
	SysRoot, ByID, ProcRoot string
}

// Discover returns hardware identities and the evidence preventing enrollment.
// An unreadable mount table protects every disk; malformed lsblk output fails
// the entire observation, so an incomplete response cannot imply an empty host.
func (d Discoverer) Discover(ctx context.Context) ([]model.Disk, error) {
	if d.Runner == nil {
		d.Runner = host.Run
	}
	if d.SysRoot == "" {
		d.SysRoot = "/sys"
	}
	if d.ByID == "" {
		d.ByID = "/dev/disk/by-id"
	}
	if d.ProcRoot == "" {
		d.ProcRoot = "/proc"
	}
	data, err := d.Runner(ctx, []string{"lsblk", "--json", "--bytes", "--paths", "--output", "NAME,KNAME,PATH,MAJ:MIN,TYPE,SIZE,MODEL,SERIAL,WWN,FSTYPE,PTTYPE,MOUNTPOINTS,RO,ROTA"})
	if err != nil {
		return nil, fmt.Errorf("discover disks: %w", err)
	}
	devices, err := decodeDevices(data)
	if err != nil {
		return nil, err
	}
	mounted, swaps, safetyErr := mountDevices(d.ProcRoot)
	aliases := hardwareAliases(d.ByID)
	result := make([]model.Disk, 0)
	seen := make(map[string]int)
	err = walk(devices, func(raw map[string]any) error {
		kind, err := textField(raw, "type")
		if err != nil || kind == "" {
			return fmt.Errorf("invalid lsblk device type")
		}
		if kind != "disk" {
			return nil
		}
		disk, canonical, err := d.inspect(raw, mounted, swaps, aliases, safetyErr)
		if err != nil {
			return err
		}
		if index, ok := seen[canonical]; ok {
			previous := &result[index]
			previous.Reasons = append(previous.Reasons, disk.Reasons...)
			if previous.Serial != disk.Serial || previous.SizeBytes != disk.SizeBytes || previous.WWN != disk.WWN {
				previous.Reasons = append(previous.Reasons, "Conflicting observations for device")
			}
			previous.Reasons = unique(previous.Reasons)
			if len(previous.Reasons) != 0 {
				previous.State = "Protected"
			}
			return nil
		}
		seen[canonical] = len(result)
		result = append(result, disk)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

func decodeDevices(data []byte) ([]any, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var report map[string]any
	if err := decoder.Decode(&report); err != nil {
		return nil, fmt.Errorf("invalid lsblk JSON output: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return nil, fmt.Errorf("invalid lsblk JSON output: trailing data")
	}
	devices, ok := report["blockdevices"].([]any)
	if !ok {
		return nil, fmt.Errorf("invalid lsblk JSON output: blockdevices must be an array")
	}
	return devices, nil
}

func walk(items []any, visit func(map[string]any) error) error {
	for _, item := range items {
		device, ok := item.(map[string]any)
		if !ok {
			return fmt.Errorf("invalid lsblk JSON output: device must be an object")
		}
		if err := visit(device); err != nil {
			return err
		}
		children, err := childrenOf(device)
		if err != nil {
			return err
		}
		if err := walk(children, visit); err != nil {
			return err
		}
	}
	return nil
}

func childrenOf(raw map[string]any) ([]any, error) {
	if raw["children"] == nil {
		return nil, nil
	}
	children, ok := raw["children"].([]any)
	if !ok {
		return nil, fmt.Errorf("invalid lsblk JSON output: children must be an array")
	}
	return children, nil
}

func textField(raw map[string]any, key string) (string, error) {
	switch value := raw[key].(type) {
	case nil:
		return "", nil
	case string:
		return value, nil
	case json.Number:
		return value.String(), nil
	default:
		return "", fmt.Errorf("invalid lsblk %s value", key)
	}
}

// lsblk versions emit booleans, numbers or strings for RO and ROTA. In
// particular, the nonempty string "0" must never be interpreted as true.
func flag(value any) (bool, bool) {
	switch value := value.(type) {
	case bool:
		return value, true
	case json.Number:
		return flag(value.String())
	case string:
		switch strings.ToLower(strings.TrimSpace(value)) {
		case "0", "false":
			return false, true
		case "1", "true":
			return true, true
		}
	}
	return false, false
}

func (d Discoverer) inspect(raw map[string]any, mounted, swaps map[string]bool, aliases map[string]string, safetyErr error) (model.Disk, string, error) {
	fields := make(map[string]string)
	for _, key := range []string{"path", "name", "serial", "wwn", "size", "model"} {
		value, err := textField(raw, key)
		if err != nil {
			return model.Disk{}, "", err
		}
		fields[key] = value
	}
	path := fields["path"]
	if path == "" {
		path = fields["name"]
	}
	if path == "" {
		return model.Disk{}, "", fmt.Errorf("invalid lsblk disk: device path is missing")
	}
	reasons := make([]string, 0)
	if safetyErr != nil {
		reasons = append(reasons, "Cannot read host mount/swap state: "+safetyErr.Error())
	}
	canonical, err := filepath.EvalSymlinks(path)
	if err != nil || !filepath.IsAbs(path) {
		canonical = path
		reasons = append(reasons, "Cannot resolve device path")
	}
	if ro, known := flag(raw["ro"]); !known {
		reasons = append(reasons, "Cannot verify device read-only state")
	} else if ro {
		reasons = append(reasons, "Read-only device")
	}
	children, err := childrenOf(raw)
	if err != nil {
		return model.Disk{}, "", err
	}
	if len(children) != 0 {
		reasons = append(reasons, "Has partitions or dependent block devices")
	}
	err = walk([]any{raw}, func(member map[string]any) error {
		values := make(map[string]string)
		for _, key := range []string{"kname", "name", "path", "fstype", "pttype", "maj:min"} {
			value, err := textField(member, key)
			if err != nil {
				return err
			}
			values[key] = value
		}
		// Null is lsblk's normal representation for no signature or mount;
		// an absent requested column instead means the evidence is incomplete.
		if _, present := member["fstype"]; !present {
			reasons = append(reasons, "Cannot verify filesystem signatures")
		}
		if _, present := member["pttype"]; !present {
			reasons = append(reasons, "Cannot verify partition table")
		}
		if _, present := member["mountpoints"]; !present {
			reasons = append(reasons, "Cannot verify device mountpoints")
		}
		if values["fstype"] != "" {
			reasons = append(reasons, "Filesystem/signature detected: "+values["fstype"])
		}
		if values["pttype"] != "" {
			reasons = append(reasons, "Partition table detected: "+values["pttype"])
		}
		hasMount, err := hasMountpoint(member["mountpoints"])
		if err != nil {
			return err
		}
		if !deviceNumber.MatchString(values["maj:min"]) {
			reasons = append(reasons, "Cannot verify device major/minor number")
		}
		if hasMount || mounted[values["maj:min"]] {
			reasons = append(reasons, "Mounted device (includes host mounts)")
		}
		memberPath, err := filepath.EvalSymlinks(values["path"])
		if (err == nil && swaps[memberPath]) || values["fstype"] == "swap" {
			reasons = append(reasons, "Swap device")
		}
		name := values["kname"]
		if name == "" {
			name = values["name"]
		}
		name = filepath.Base(name)
		if name == "." || name == string(filepath.Separator) {
			reasons = append(reasons, "Cannot verify device holders")
			return nil
		}
		holders, err := os.ReadDir(filepath.Join(d.SysRoot, "class/block", name, "holders"))
		if err != nil {
			reasons = append(reasons, "Cannot verify device holders")
		} else if len(holders) != 0 {
			reasons = append(reasons, "Device has active holders (RAID/LVM/multipath)")
		}
		return nil
	})
	if err != nil {
		return model.Disk{}, "", err
	}
	serial := strings.TrimSpace(fields["serial"])
	if serial == "" {
		reasons = append(reasons, "Stable serial number is missing")
	}
	stable := aliases[canonical]
	if stable == "" {
		reasons = append(reasons, "Stable /dev/disk/by-id path is missing")
		stable = path
	}
	var size int64
	if fields["size"] != "" {
		size, err = strconv.ParseInt(fields["size"], 10, 64)
		if err != nil {
			return model.Disk{}, "", fmt.Errorf("invalid lsblk disk size: %w", err)
		}
	}
	if size < 64*1024*1024 {
		reasons = append(reasons, "Device is smaller than 64 MiB")
	}
	identity := fields["wwn"]
	if identity == "" {
		identity = serial
	}
	if identity == "" {
		identity = path
	}
	hash := sha256.Sum256([]byte(identity))
	media := "Unknown"
	if strings.HasPrefix(filepath.Base(path), "nvme") {
		media = "NVMe"
	} else if rotating, known := flag(raw["rota"]); known {
		media = "SSD"
		if rotating {
			media = "HDD"
		}
	}
	state := "Available"
	if len(reasons) != 0 {
		state = "Protected"
	}
	return model.Disk{ID: "disk-" + hex.EncodeToString(hash[:8]), Name: filepath.Base(path), Path: stable, DevicePath: path,
		Serial: serial, WWN: fields["wwn"], SizeBytes: size, Model: strings.TrimSpace(fields["model"]),
		Media: media, State: state, Reasons: unique(reasons)}, canonical, nil
}

func hasMountpoint(value any) (bool, error) {
	switch value := value.(type) {
	case nil:
		return false, nil
	case string:
		return value != "", nil
	case []any:
		mounted := false
		for _, item := range value {
			if item == nil {
				continue
			}
			path, ok := item.(string)
			if !ok {
				return false, fmt.Errorf("invalid lsblk mountpoints value")
			}
			mounted = mounted || path != ""
		}
		return mounted, nil
	default:
		return false, fmt.Errorf("invalid lsblk mountpoints value")
	}
}

func hardwareAliases(root string) map[string]string {
	aliases := make(map[string]string)
	entries, err := os.ReadDir(root)
	if err != nil {
		return aliases
	}
	sort.Slice(entries, func(i, j int) bool {
		first, second := entries[i].Name(), entries[j].Name()
		if strings.HasPrefix(first, "wwn-") != strings.HasPrefix(second, "wwn-") {
			return strings.HasPrefix(first, "wwn-")
		}
		return first < second
	})
	for _, entry := range entries {
		if entry.Type()&os.ModeSymlink == 0 || strings.HasPrefix(entry.Name(), "lvm-pv-uuid-") || partitionAlias.MatchString(entry.Name()) {
			continue
		}
		alias := filepath.Join(root, entry.Name())
		target, err := filepath.EvalSymlinks(alias)
		if err == nil && aliases[target] == "" {
			aliases[target] = alias
		}
	}
	return aliases
}

func mountDevices(procRoot string) (map[string]bool, map[string]bool, error) {
	mounted, swaps := make(map[string]bool), make(map[string]bool)
	for _, source := range []string{"1/mountinfo", "self/mountinfo"} {
		data, err := os.ReadFile(filepath.Join(procRoot, source))
		if err != nil {
			return mounted, swaps, err
		}
		for _, line := range strings.Split(string(data), "\n") {
			if strings.TrimSpace(line) == "" {
				continue
			}
			fields := strings.Fields(line)
			if len(fields) < 10 || !deviceNumber.MatchString(fields[2]) {
				return mounted, swaps, fmt.Errorf("malformed %s", source)
			}
			separator := -1
			for i := 6; i < len(fields); i++ {
				if fields[i] == "-" {
					separator = i
					break
				}
			}
			if separator < 0 || len(fields)-separator < 4 {
				return mounted, swaps, fmt.Errorf("malformed %s", source)
			}
			mounted[fields[2]] = true
		}
	}
	data, err := os.ReadFile(filepath.Join(procRoot, "swaps"))
	if err != nil {
		return mounted, swaps, err
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	header := strings.Fields(lines[0])
	if len(header) != 5 || strings.Join(header, " ") != "Filename Type Size Used Priority" {
		return mounted, swaps, fmt.Errorf("malformed swaps header")
	}
	unescape := strings.NewReplacer(`\040`, " ", `\011`, "\t", `\012`, "\n", `\134`, `\`)
	for _, line := range lines[1:] {
		if strings.TrimSpace(line) == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 5 {
			return mounted, swaps, fmt.Errorf("malformed swaps entry")
		}
		switch fields[1] {
		case "file":
			// With hostPID, /proc/swaps describes the host while path lookup
			// still uses the agent container's mount namespace. A host swapfile
			// need not be visible here; host mountinfo protects its backing disk.
			continue
		case "partition":
			// Block devices are exposed through the host /dev mount and must
			// resolve, including aliases, before their identity can be trusted.
		default:
			return mounted, swaps, fmt.Errorf("unknown swap type %q", fields[1])
		}
		path, err := filepath.EvalSymlinks(unescape.Replace(fields[0]))
		if err != nil {
			return mounted, swaps, fmt.Errorf("cannot resolve swap device: %w", err)
		}
		swaps[path] = true
	}
	return mounted, swaps, nil
}

func unique(values []string) []string {
	result := make([]string, 0, len(values))
	seen := make(map[string]bool, len(values))
	for _, value := range values {
		if !seen[value] {
			seen[value] = true
			result = append(result, value)
		}
	}
	return result
}
