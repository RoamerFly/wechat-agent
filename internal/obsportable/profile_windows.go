//go:build windows

package obsportable

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"

	"wechat-bot/internal/workspacefs"
)

const profileName = "Station"

func writeProfile(root *os.Root, port int, ticket string) error {
	if info, err := root.Lstat("config"); err == nil {
		if !info.IsDir() || info.Mode()&fs.ModeSymlink != 0 {
			return ErrInvalid
		}
		// Only this runtime's generated portable profile is mutable. The
		// exclusive session lock prevents another process using it here.
		if err := root.RemoveAll("config"); err != nil {
			return ErrInvalid
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return ErrInvalid
	}
	profile := filepath.FromSlash("config/obs-studio/basic/profiles/" + profileName)
	scenes := filepath.FromSlash("config/obs-studio/basic/scenes")
	websocket := filepath.FromSlash("config/obs-studio/plugin_config/obs-websocket")
	for _, directory := range []string{profile, scenes, websocket} {
		created, err := workspacefs.EnsureDirectory(root, directory)
		if err != nil {
			return ErrInvalid
		}
		_ = created.Close()
	}
	service, err := loopbackService(port, ticket)
	if err != nil {
		return ErrInvalid
	}
	for name, contents := range map[string][]byte{
		filepath.Join(profile, "service.json"):             service,
		filepath.Join(profile, "basic.ini"):                []byte(basicProfile),
		filepath.Join(websocket, "config.json"):            []byte(disabledWebSocket),
		filepath.Join(scenes, profileName+".json"):         []byte(sceneCollection),
		filepath.FromSlash("config/obs-studio/user.ini"):   []byte(userProfile),
		filepath.FromSlash("config/obs-studio/global.ini"): []byte(globalProfile),
	} {
		if err := root.WriteFile(name, contents, 0600); err != nil {
			return ErrInvalid
		}
	}
	return nil
}

const disabledWebSocket = `{"first_load":false,"server_enabled":false,"auth_required":true,"alerts_enabled":false}`

const globalProfile = `
[General]
LastVersion=553648129
EnableAutoUpdates=false
`

const userProfile = `
[General]
FirstRun=true
ConfirmOnExit=false

[Basic]
Profile=Station
ProfileDir=Station
SceneCollection=Station
SceneCollectionFile=Station
ConfigOnNewProfile=false
`

const basicProfile = `
[General]
Name=Station

[Output]
Mode=Simple
Reconnect=false

[SimpleOutput]
StreamEncoder=x264
StreamAudioEncoder=aac
VBitrate=1400
ABitrate=128
Preset=veryfast

[Video]
BaseCX=1280
BaseCY=720
OutputCX=1280
OutputCY=720
FPSType=0
FPSCommon=30

[Audio]
SampleRate=48000
ChannelSetup=Stereo
`

// Only a bundled solid-color source is configured. The generated collection
// contains no capture devices, browser sources, scripts or external files.
const sceneCollection = `{
 "name":"Station",
 "current_scene":"Station Program",
 "current_program_scene":"Station Program",
 "scene_order":[{"name":"Station Program"}],
 "sources":[
  {
   "id":"color_source",
   "versioned_id":"color_source_v3",
   "name":"Station Background",
   "uuid":"7d03a4e1-3342-43da-9ca4-c31d8926af59",
   "enabled":true,
   "settings":{"color":4283049752,"width":1280,"height":720}
  },
  {
   "id":"scene",
   "versioned_id":"scene",
   "name":"Station Program",
   "uuid":"7448a9ba-3607-4e62-8f1b-1341899a3890",
   "enabled":true,
   "settings":{"items":[{
    "id":1,"name":"Station Background","source_uuid":"7d03a4e1-3342-43da-9ca4-c31d8926af59",
    "visible":true,"pos":{"x":0,"y":0},"scale":{"x":1,"y":1}
   }]}
  }
 ]
}
`
