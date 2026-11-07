package mediaspec

// VoiceFile and VoiceEntry preserve the registered voice catalog format.
type VoiceFile struct {
	Relative string `json:"relative_path,omitempty"`
	File     string `json:"file,omitempty"`
	Path     string `json:"path,omitempty"`
	Filename string `json:"filename"`
	Hash     string `json:"sha256"`
	Bytes    int64  `json:"bytes"`
	Kind     string `json:"kind,omitempty"`
	Epoch    int    `json:"epoch,omitempty"`
}
type VoiceEntry struct {
	Owner         string            `json:"owner_id,omitempty"`
	Role          string            `json:"role,omitempty"`
	Directory     string            `json:"directory,omitempty"`
	ID            string            `json:"id"`
	Name          string            `json:"name"`
	Engine        string            `json:"engine"`
	Status        string            `json:"status"`
	Notes         string            `json:"notes"`
	Weights       []VoiceFile       `json:"weights"`
	References    []VoiceFile       `json:"references"`
	Previews      []VoiceFile       `json:"previews"`
	Recommended   map[string]string `json:"recommended_weights,omitempty"`
	Instruction   string            `json:"synthesis_instruction,omitempty"`
	Configuration string            `json:"configuration,omitempty"`
	ProfileID     string            `json:"profile_id,omitempty"`
	Public        bool              `json:"-"`
}
