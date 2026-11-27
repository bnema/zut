package subagents

import "strings"

func residentScopeIDs(ids []string) map[string]struct{} {
	scope := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		if id != "" {
			scope[id] = struct{}{}
		}
	}
	return scope
}

// ScopedSnapshots filters ownership before projecting live children. An empty
// scope matches nothing, including children spawned by the current manager.
func (m *ResidentManager) ScopedSnapshots(ids []string) []ResidentSnapshot {
	scope := residentScopeIDs(ids)
	if m == nil || len(scope) == 0 {
		return nil
	}
	var snapshots []ResidentSnapshot
	var live []*ResidentChild
	m.mu.Lock()
	for id, child := range m.children {
		if _, ok := scope[child.spec.ParentSessionID]; ok || m.spawnedHere[id] {
			live = append(live, child)
		}
	}
	for id, snapshot := range m.recovered {
		if _, exists := m.children[id]; exists {
			continue
		}
		if _, ok := scope[snapshot.ParentSessionID]; ok || m.spawnedHere[id] {
			snapshots = append(snapshots, snapshot)
		}
	}
	m.mu.Unlock()
	for _, child := range live {
		snapshots = append(snapshots, residentSnapshot(child))
	}
	return snapshots
}

// ResolveScopedSnapshot prefers exact in-scope IDs, then a unique in-scope
// prefix. Only the resolved live child is projected.
func (m *ResidentManager) ResolveScopedSnapshot(ids []string, id string) (ResidentSnapshot, bool) {
	scope := residentScopeIDs(ids)
	if m == nil || len(scope) == 0 || id == "" {
		return ResidentSnapshot{}, false
	}
	m.mu.Lock()
	child, snapshot, ok := m.resolveScopedLocked(scope, id)
	m.mu.Unlock()
	if child != nil {
		return residentSnapshot(child), true
	}
	return snapshot, ok
}

func (m *ResidentManager) resolveScopedLocked(scope map[string]struct{}, id string) (*ResidentChild, ResidentSnapshot, bool) {
	lookup := func(key string) (*ResidentChild, ResidentSnapshot, bool) {
		if child := m.children[key]; child != nil {
			_, ok := scope[child.spec.ParentSessionID]
			return child, ResidentSnapshot{}, ok || m.spawnedHere[key]
		}
		snapshot, exists := m.recovered[key]
		_, ok := scope[snapshot.ParentSessionID]
		return nil, snapshot, exists && (ok || m.spawnedHere[key])
	}
	if child, snapshot, ok := lookup(id); ok {
		return child, snapshot, true
	}
	var child *ResidentChild
	var snapshot ResidentSnapshot
	found := false
	consider := func(key string) bool {
		if !strings.HasPrefix(key, id) {
			return true
		}
		c, s, ok := lookup(key)
		if !ok {
			return true
		}
		if found {
			return false
		}
		child, snapshot, found = c, s, true
		return true
	}
	for key := range m.children {
		if !consider(key) {
			return nil, ResidentSnapshot{}, false
		}
	}
	for key := range m.recovered {
		if m.children[key] == nil && !consider(key) {
			return nil, ResidentSnapshot{}, false
		}
	}
	return child, snapshot, found
}
