package station

import (
	"context"
	"errors"
	"io"
	"path/filepath"

	"wechat-bot/internal/companion"
	"wechat-bot/internal/mcptools"
	"wechat-bot/internal/stationservices"
)

func (h *localHomeService) applicationOwner() (stationservices.Owner, error) {
	record, err := h.identity.Read()
	if err != nil || record.LocalOwner == nil {
		return stationservices.Owner{}, errors.New("本地主人身份不可用")
	}
	return stationservices.Owner{FishID: record.FishID, ID: record.LocalOwner.ID}, nil
}

func (s *localModelStore) applicationModel(app string) stationservices.Model {
	return stationservices.Model{
		Persona: func() (string, error) {
			view, err := s.View()
			if err != nil || !view.KeyConfigured {
				return "", errors.New("本机模型尚未就绪")
			}
			_, err = (localAppPersonaStore{dir: filepath.Dir(s.path)}).View(app)
			return "", err
		},
		Client: localApplicationModel{store: s, app: app},
	}
}

type localApplicationModel struct {
	store *localModelStore
	app   string
}

func (m localApplicationModel) prepare(system string) (companion.LanguageModel, string, error) {
	model, settings, err := m.store.ClientSnapshot()
	if err != nil {
		return nil, "", err
	}
	profile, err := (localAppPersonaStore{dir: filepath.Dir(m.store.path)}).View(m.app)
	if err != nil {
		return nil, "", err
	}
	return model, system + applicationPersonaPolicy(settings.Persona, profile), nil
}

func (m localApplicationModel) Chat(ctx context.Context, system string, history []map[string]string) (string, error) {
	model, system, err := m.prepare(system)
	if err != nil {
		return "", err
	}
	return model.Chat(ctx, system, history)
}

func (m localApplicationModel) ChatFunctions(ctx context.Context, system string, history []map[string]string, tools []mcptools.Tool) (string, error) {
	model, system, err := m.prepare(system)
	if err != nil {
		return "", err
	}
	native, ok := model.(interface {
		ChatFunctions(context.Context, string, []map[string]string, []mcptools.Tool) (string, error)
	})
	if !ok {
		return "", errors.New("station_model_functions_unavailable")
	}
	return native.ChatFunctions(ctx, system, history, tools)
}

func (s *localArtifactStore) HomeRevision() (uint64, error) {
	grant, err := (&localWorkspaceAccess{home: s.home}).resolve()
	return grant.Revision, err
}

// Only stream methods cross the app boundary. In particular, the opened file
// must not expose its private on-disk name or file descriptor.
type applicationArtifactReader struct{ io.ReadCloser }

func (s *localArtifactStore) ReadArtifact(id string) (stationservices.Artifact, error) {
	a, err := s.read(id)
	if err != nil {
		return stationservices.Artifact{}, err
	}
	ref := a.applicationArtifact()
	ref.OpenVerified = func(ctx context.Context, stillAllowed func() bool) (io.ReadCloser, error) {
		if _, ok := s.access(a); !ok || stillAllowed == nil || !stillAllowed() {
			return nil, errors.New("artifact_source_or_home_changed")
		}
		file, err := s.open(ctx, a, stillAllowed)
		if err != nil {
			return nil, err
		}
		if _, ok := s.access(a); !ok {
			file.Close()
			return nil, errors.New("artifact_source_or_home_changed")
		}
		return applicationArtifactReader{file}, nil
	}
	return ref, nil
}

func (a localArtifact) applicationArtifact() stationservices.Artifact {
	ref := stationservices.Artifact{ID: a.ID, Name: a.Name, SHA256: a.SHA256, Bytes: a.Bytes}
	if a.Parent != nil {
		ref.InputArtifactID, ref.InputSHA256 = a.Parent.ID, a.Parent.SHA256
	}
	return ref
}
