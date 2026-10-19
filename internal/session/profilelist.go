package session

// ProfileList is the profiles sessions can run under, by name only, in their
// configured order. It is the GET /profiles body.
type ProfileList struct {
	Profiles []ProfileName `json:"profiles"`
	Default  string        `json:"default"`
}

// ProfileName is one entry of a ProfileList.
type ProfileName struct {
	Name    string `json:"name"`
	Default bool   `json:"default"`
}

// ProfileList lists the profiles and which is the default. It names no dir.
func (m *Manager) ProfileList() ProfileList {
	def, _ := m.lookupProfile("")
	list := m.profiles()
	out := ProfileList{Profiles: make([]ProfileName, 0, len(list)), Default: def.Name}
	for _, p := range list {
		out.Profiles = append(out.Profiles, ProfileName{Name: p.Name, Default: p.Name == def.Name})
	}
	return out
}
