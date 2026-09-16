package plan

import "sort"

// The change-oriented half of the plan (resource_changes, resource_drift) says
// what this run touches. Checks that ask "what is out there?" - a retiring SKU
// nobody is changing - need the whole estate instead, which the plan also
// carries:
//
//	planned_values.root_module   what exists once this plan is applied
//	prior_state.values.root_module   what exists now, before it
//
// Both are whole-state snapshots with every attribute, so reading them costs no
// extra Terraform run and no cloud access.

// State is a state representation nested in the plan (prior_state).
type State struct {
	Values *StateValues `json:"values"`
}

// StateValues holds the module tree of a state or of planned_values.
type StateValues struct {
	RootModule Module `json:"root_module"`
}

// Module is one node of that tree. The root module has no address.
type Module struct {
	Address      string          `json:"address"`
	Resources    []StateResource `json:"resources"`
	ChildModules []Module        `json:"child_modules"`
}

// StateResource is one resource instance. Address is fully qualified, module
// prefix and index included, e.g. module.network.azurerm_public_ip.egress[0].
// Index is a number for count and a string for for_each, and nil for neither.
type StateResource struct {
	Address       string         `json:"address"`
	Mode          string         `json:"mode"`
	Type          string         `json:"type"`
	Name          string         `json:"name"`
	Index         any            `json:"index"`
	ProviderName  string         `json:"provider_name"`
	Values        map[string]any `json:"values"`
	ModuleAddress string         `json:"-"`
}

// Resources returns every managed resource in the estate, sorted by address.
//
// planned_values wins when the plan has it: it is the estate as it will be,
// so a resource this run fixes is reported in its fixed state, and one this run
// destroys is not reported at all. prior_state is the fallback for a plan
// without planned_values. Data sources are left out - they are a read of
// someone else's infrastructure, not ours to act on.
//
// A plan carrying neither section (a trimmed fixture, an old Terraform) yields
// no resources and no error: a check that needs them says so itself.
func (p *Plan) Resources() []StateResource {
	values := p.PlannedValues
	if values == nil && p.PriorState != nil {
		values = p.PriorState.Values
	}
	if values == nil {
		return nil
	}
	var out []StateResource
	collect(&values.RootModule, &out)
	sort.Slice(out, func(i, j int) bool { return out[i].Address < out[j].Address })
	return out
}

func collect(m *Module, out *[]StateResource) {
	for _, r := range m.Resources {
		if r.Mode != "managed" {
			continue
		}
		r.ModuleAddress = m.Address
		*out = append(*out, r)
	}
	for i := range m.ChildModules {
		collect(&m.ChildModules[i], out)
	}
}
