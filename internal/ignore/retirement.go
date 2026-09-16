package ignore

import (
	"strings"

	"github.com/wes-key/tf-snag/internal/report"
)

// Retirement rules waive a dated deadline rather than a change, so they are
// written against the catalogue entry - by id, the stable name the entry keeps
// for life, or by resource address for a team that has migrated everything but
// one instance:
//
//	retirements:
//	  - id: azure-public-ip-basic-sku
//	    reason: CSES deployment, exempt from the retirement
//	  - address: module.legacy.azurerm_lb.edge
//	    reason: decommissioned in Q3, tracked in PLAT-412
//
// An address rule drops that instance from the finding. The finding itself is
// suppressed only when every instance has gone: a waiver for one resource must
// not silence the other nine.
func (s *Set) applyRetirements(r *report.Report) {
	for i := range r.Retirements {
		rt := &r.Retirements[i]
		if rule, ok := s.matchRetirementID(rt.ID); ok {
			mark(&rt.Suppressed, &rt.SuppressReason, &rt.SuppressSrc, &rt.SuppressKind, rule)
			s.record(rule, Hit{Kind: "retirement", Address: rt.ID})
			continue
		}
		kept := rt.Instances[:0]
		var last Rule
		var dropped int
		for _, in := range rt.Instances {
			rule, ok := s.matchRetirementAddr(in.Address)
			if !ok {
				kept = append(kept, in)
				continue
			}
			dropped++
			last = rule
			s.record(rule, Hit{Kind: "retirement", Address: in.Address})
		}
		rt.Instances = kept
		if dropped > 0 && len(rt.Instances) == 0 {
			mark(&rt.Suppressed, &rt.SuppressReason, &rt.SuppressSrc, &rt.SuppressKind, last)
		}
	}
}

func (s *Set) matchRetirementID(id string) (Rule, bool) {
	for _, r := range s.retire {
		if r.Match != "" && strings.EqualFold(r.Match, id) {
			return r, true
		}
	}
	return Rule{}, false
}

func (s *Set) matchRetirementAddr(addr string) (Rule, bool) {
	for _, r := range s.retire {
		if r.Addr != "" && (globMatch(r.Addr, addr) || globMatch(r.Addr, typeName(addr))) {
			return r, true
		}
	}
	return Rule{}, false
}
