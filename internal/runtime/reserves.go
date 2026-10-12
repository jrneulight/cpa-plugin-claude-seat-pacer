package runtime

import (
	"fmt"

	"github.com/yuya-iwabuchi/cpa-plugin-claude-seat-pacer/internal/model"
)

// entryCeilings is the reserve ceilings cfg sets on one credential, matched
// on its id and, when the host lists it, its file name, account email, host
// label and operator note. nil when no reserve names it.
func entryCeilings(cfg model.Config, id string, entry HostAuthFileEntry, listed bool) model.Ceilings {
	if len(cfg.Reserves) == 0 {
		return nil
	}
	if !listed {
		return cfg.CeilingsFor(id)
	}
	return cfg.CeilingsFor(id, entry.Name, entry.Email, entry.Label, entry.Note)
}

// reserveCeilings is the reserve ceilings on each of ids that a reserve names,
// and nil when cfg sets no reserve, so a pool with none pays nothing for the
// feature. It holds p.mu only to copy the host's credential list.
func (p *Plugin) reserveCeilings(cfg model.Config, ids []string) map[string]model.Ceilings {
	if len(cfg.Reserves) == 0 {
		return nil
	}
	p.mu.Lock()
	entries := make(map[string]HostAuthFileEntry, len(p.auths))
	for _, entry := range p.auths {
		entries[authID(entry)] = entry
	}
	p.mu.Unlock()

	out := make(map[string]model.Ceilings, len(ids))
	for _, id := range ids {
		entry, listed := entries[id]
		if c := entryCeilings(cfg, id, entry, listed); c != nil {
			out[id] = c
		}
	}
	return out
}

// reserveWarnings names each reserve account that matches no credential the
// host lists, since a mistyped account reserves nothing and says nothing. It
// is silent until the host has listed a credential, and names an account by
// its position, never its text, which is routinely an address.
func reserveWarnings(cfg model.Config, auths []HostAuthFileEntry) []string {
	if len(cfg.Reserves) == 0 || len(auths) == 0 {
		return nil
	}
	var warnings []string
	for i, r := range cfg.Reserves {
		for j, account := range r.Accounts {
			one := model.Config{Reserves: []model.ReserveConfig{{Accounts: []string{account}, Ceilings: r.Ceilings}}}
			matched := false
			for _, entry := range auths {
				if entryCeilings(one, authID(entry), entry, true) != nil {
					matched = true
					break
				}
			}
			if !matched {
				warnings = append(warnings, fmt.Sprintf(
					"reserves[%d].accounts[%d] matches no credential the host lists, so its ceilings hold nothing back", i, j))
			}
		}
	}
	return warnings
}
