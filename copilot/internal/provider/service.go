package provider

import (
	"sync"
	"time"

	"github.com/BierDav/cliproxyapi-plugins/framework"
)

// ProviderID is the host provider key and credential type the plugin owns.
// CLIProxyAPI 8.1 and later ship a native "copilot" provider whose executor
// always takes precedence over a plugin executor with the same key, so the
// plugin uses its own key.
const ProviderID = "github-copilot"

const providerID = ProviderID

type Service struct {
	host framework.Transport
	now  func() time.Time

	configMu sync.RWMutex
	config   Config

	oauthMu      sync.Mutex
	oauthSession map[string]*deviceSession

	tokenMu       sync.Mutex
	tokenEntries  map[string]copilotTokenEntry
	tokenInflight map[string]*tokenFlight

	modelMu      sync.Mutex
	modelEntries map[string]modelCacheEntry
	// unsupported holds, per auth and model, until when GitHub refused the
	// model for that account although its catalog lists it.
	unsupported map[string]time.Time
}

func New(host framework.Transport) *Service {
	return &Service{
		host:          host,
		now:           time.Now,
		config:        DefaultConfig(),
		oauthSession:  make(map[string]*deviceSession),
		tokenEntries:  make(map[string]copilotTokenEntry),
		tokenInflight: make(map[string]*tokenFlight),
		modelEntries:  make(map[string]modelCacheEntry),
		unsupported:   make(map[string]time.Time),
	}
}

func (s *Service) Configure(raw []byte) error {
	cfg, errParse := ParseConfig(raw)
	if errParse != nil {
		return errParse
	}
	s.configMu.Lock()
	s.config = cfg
	s.configMu.Unlock()
	s.modelMu.Lock()
	clear(s.modelEntries)
	s.modelMu.Unlock()
	return nil
}

func (s *Service) Config() Config {
	s.configMu.RLock()
	defer s.configMu.RUnlock()
	return s.config
}

func (s *Service) Shutdown() {
	s.oauthMu.Lock()
	clear(s.oauthSession)
	s.oauthMu.Unlock()
	s.tokenMu.Lock()
	clear(s.tokenEntries)
	s.tokenMu.Unlock()
	s.modelMu.Lock()
	clear(s.modelEntries)
	clear(s.unsupported)
	s.modelMu.Unlock()
}
