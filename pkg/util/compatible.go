package util

import (
	"regexp"
	"strings"

	"github.com/NVIDIA/go-nvml/pkg/nvml"
)

// GetCompatibleNVMLMemoryInfo wraps nvml.Device.GetMemoryInfo().
//
// Some environments/drivers return nvml.ERROR_NOT_SUPPORTED for GetMemoryInfo.
// In that case, we fall back to GetName() and derive total memory by matching
// the device name against a (currently hardcoded) mapping table.
//
// Fallback behavior:
//   - Total: derived from the mapping table (bytes)
//   - Free:  0
//   - Used:  Total
//
// NOTE: The mapping table is intentionally hardcoded for now; later it can be
// moved to configuration.
func GetCompatibleNVMLMemoryInfo(dev nvml.Device) (nvml.Memory, nvml.Return) {
	mem, ret := dev.GetMemoryInfo()
	if ret == nvml.SUCCESS || ret != nvml.ERROR_NOT_SUPPORTED {
		return mem, ret
	}

	name, nret := dev.GetName()
	if nret != nvml.SUCCESS {
		return mem, nret
	}

	config, ok := GetCompatibleConfigsByDeviceName(name)
	if !ok {
		return mem, ret
	}
	return nvml.Memory{
		Total: config.TotalMemory,
		Free:  0,
		Used:  config.TotalMemory,
	}, nvml.SUCCESS
}

type DeviceCompatibleConfigPattern struct {
	Pattern           *regexp.Regexp
	TotalMemory       uint64 // bytes
	DefaultShareMode  string
	AllowedShareModes []string
}

var compatibleConfigPatterns = []DeviceCompatibleConfigPattern{
	{Pattern: regexp.MustCompile(`^NVIDIA\s+GB10$`), TotalMemory: 96 * 1024 * 1024 * 1024, DefaultShareMode: ShareModeMemSlicing, AllowedShareModes: []string{ShareModeMemSlicing, ShareModeExclusive}},
}

func GetCompatibleConfigsByDeviceName(name string) (DeviceCompatibleConfigPattern, bool) {
	n := strings.TrimSpace(name)
	for _, rule := range compatibleConfigPatterns {
		if rule.Pattern.MatchString(n) {
			return rule, true
		}
	}
	return DeviceCompatibleConfigPattern{}, false
}
