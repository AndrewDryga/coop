package box

import (
	_ "embed"
	"encoding/json"
	"fmt"

	agents "github.com/AndrewDryga/coop/internal/agent"
	"github.com/AndrewDryga/coop/internal/config"
)

//go:embed config_publication.js
var configPublicationJS string

var configPublicationShell = "if [ -n \"$COOP_CONFIG_PUBLICATION\" ]; then\n  /usr/local/bin/node <<'CONFIG_PUBLICATION' || exit 1\n" + configPublicationJS + "\nCONFIG_PUBLICATION\nfi\nunset COOP_CONFIG_PUBLICATION\n"

func prepareConfigPublication(cfg *config.Config, spec RunSpec) (string, error) {
	var publications []agents.ConfigPublication
	for _, name := range credentialScope(cfg, spec) {
		ag, ok := agents.Get(name)
		if !ok {
			continue
		}
		files, err := ag.DefaultsPublication(cfg)
		if err != nil {
			return "", fmt.Errorf("prepare %s config publication: %w", name, err)
		}
		publications = append(publications, files...)
	}
	if len(publications) == 0 {
		return "", nil
	}
	data, err := json.Marshal(publications)
	return string(data), err
}
