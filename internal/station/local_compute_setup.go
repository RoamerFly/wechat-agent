package station

import (
	"errors"
	"os"
	"strings"
)

// Keep filesystem and provider details out of the public tool state. Missing
// registration, corrupt registration and changed resources need distinct fixes.
func readLocalComputeRegistration(path, prefix string, limit int64) ([]byte, error) {
	data, err := localComfyServiceReadRegular(path, limit)
	if err != nil {
		if _, statErr := os.Lstat(path); os.IsNotExist(statErr) {
			return nil, errors.New(prefix + "not_installed_or_authorized")
		}
		return nil, errors.New(prefix + "registration_unreadable")
	}
	return data, nil
}

func localComputeSetupReason(id string, err error) string {
	name := "ComfyUI"
	if id == localYuEPackage {
		name = "YuE"
	}
	if err != nil {
		switch strings.TrimPrefix(err.Error(), "station_"+id+"_") {
		case "not_installed_or_authorized":
			return "尚未登记并授权本机 " + name + " 后端。"
		case "registration_unreadable":
			return name + " 部署登记无法读取，请核对文件权限和文件类型。"
		case "registration_invalid":
			return name + " 部署登记格式或参数无效，请修正登记。"
		case "scope_overlaps_agent_home":
			return name + " 运行资源须独立于大肥鱼可编辑的家目录。"
		case "source_invalid", "source_unavailable":
			return name + " 运行文件缺失或路径无效，请核对解释器和入口文件。"
		case "device_invalid":
			return name + " 计算设备登记无效，请核对 CPU 或 GPU 选择。"
		case "deployment_pin_invalid":
			return name + " 运行文件完整性校验失败，请核对审核清单和文件版本。"
		case "workflow_pins_invalid":
			return name + " 工作流或媒体工具校验失败，请核对已登记的版本。"
		}
	}
	return "请检查本机 " + name + " 运行环境和宿主部署登记。"
}
