package mirageecs

import (
	"fmt"

	"github.com/winebarrel/cronplan"
)

type Purge struct {
	Schedule string           `json:"schedule" yaml:"schedule"`
	Request  *APIPurgeRequest `json:"request" yaml:"request"`

	PurgeParams *PurgeParams         `json:"-" yaml:"-"`
	Cron        *cronplan.Expression `json:"-" yaml:"-"`
}

func (p *Purge) Validate() error {
	cron, err := cronplan.Parse(p.Schedule)
	if err != nil {
		return fmt.Errorf("invalid schedule expression %s: %w", p.Schedule, err)
	}
	p.Cron = cron

	if p.Request == nil {
		return fmt.Errorf("purge request is required")
	}
	purgeParams, err := p.Request.Validate()
	if err != nil {
		return fmt.Errorf("invalid purge request: %w", err)
	}
	p.PurgeParams = purgeParams

	return nil
}
