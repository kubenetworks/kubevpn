package client

import (
	"github.com/DataDog/go-tuf/data"
	"github.com/DataDog/go-tuf/pkg/targets"
	"github.com/DataDog/go-tuf/util"
	"github.com/DataDog/go-tuf/verify"
)

type delegatedTargetsCache struct {
	meta map[string]*data.Targets
}

func newDelegatedTargetsCache() *delegatedTargetsCache {
	return &delegatedTargetsCache{
		meta: make(map[string]*data.Targets),
	}
}

func (c *delegatedTargetsCache) loadDelegatedTargets(client *Client, snapshot *data.Snapshot, role string, db *verify.DB) (*data.Targets, error) {
	if t, ok := c.meta[role]; ok {
		return t, nil
	}

	targets, err := client.loadDelegatedTargets(snapshot, role, db)
	if err != nil {
		return nil, err
	}

	c.meta[role] = targets
	return targets, nil
}

// getTargetFileMeta searches for a verified TargetFileMeta matching a target
// Requires a local snapshot to be loaded and is locked to the snapshot versions.
func (c *Client) getTargetFileMeta(target string) (data.TargetFileMeta, error) {
	metas, err := c.getTargetFileMetas([]string{target})
	if err != nil {
		return data.TargetFileMeta{}, err
	}
	return metas[target], nil
}

func (c *Client) getTargetFileMetas(targets []string) (data.TargetFiles, error) {
	snapshot, err := c.loadLocalSnapshot()
	if err != nil {
		return nil, err
	}
	cache := newDelegatedTargetsCache()
	targetFileMetas := make(data.TargetFiles, len(targets))
	for _, target := range targets {
		normalizedTarget := util.NormalizeTarget(target)
		targetFileMeta, _, err := c.getTargetFileMetaDelegationPath(normalizedTarget, snapshot, cache)
		if _, ok := err.(ErrUnknownTarget); ok {
			return nil, ErrUnknownTarget{target, snapshot.Version}
		}
		if err != nil {
			return nil, err
		}
		targetFileMetas[target] = targetFileMeta
	}
	return targetFileMetas, nil
}

// getTargetFileMetaDelegationPath searches for a verified TargetFileMeta matching a target
// Requires snapshot to be passed and is locked to that specific snapshot versions.
// Searches through delegated targets following TUF spec 1.0.19 section 5.6.
func (c *Client) getTargetFileMetaDelegationPath(target string, snapshot *data.Snapshot, cache *delegatedTargetsCache) (data.TargetFileMeta, []string, error) {
	// delegationsIterator covers 5.6.7
	// - pre-order depth-first search starting with the top targets
	// - filter delegations with paths or path_hash_prefixes matching searched target
	// - 5.6.7.1 cycles protection
	// - 5.6.7.2 terminations
	delegations, err := targets.NewDelegationsIterator(target, c.db)
	if err != nil {
		return data.TargetFileMeta{}, nil, err
	}

	targetFileMeta := data.TargetFileMeta{}
	delegationRole := ""

	for i := 0; i < c.MaxDelegations; i++ {
		d, ok := delegations.Next()
		if !ok {
			return data.TargetFileMeta{}, nil, ErrUnknownTarget{target, snapshot.Version}
		}

		// covers 5.6.{1,2,3,4,5,6}
		targets, err := cache.loadDelegatedTargets(c, snapshot, d.Delegatee.Name, d.DB)
		if err != nil {
			return data.TargetFileMeta{}, nil, err
		}

		// stop when the searched TargetFileMeta is found
		if m, ok := targets.Targets[target]; ok {
			delegationRole = d.Delegatee.Name
			targetFileMeta = m
			break
		}

		if targets.Delegations != nil {
			delegationsDB, err := verify.NewDBFromDelegations(targets.Delegations)
			if err != nil {
				return data.TargetFileMeta{}, nil, err
			}
			err = delegations.Add(targets.Delegations.Roles, d.Delegatee.Name, delegationsDB)
			if err != nil {
				return data.TargetFileMeta{}, nil, err
			}
		}
	}

	if len(delegationRole) > 0 {
		return targetFileMeta, buildPath(delegations.Parent, delegationRole, ""), nil
	}

	return data.TargetFileMeta{}, nil, ErrMaxDelegations{
		Target:          target,
		MaxDelegations:  c.MaxDelegations,
		SnapshotVersion: snapshot.Version,
	}
}

func buildPath(parent func(string) string, start string, end string) []string {
	if start == end {
		return nil
	}

	path := []string{start}
	current := start
	for {
		current = parent(current)
		if current == end {
			break
		}
		path = append(path, current)
	}
	return path
}

func (c *Client) loadLocalSnapshot() (*data.Snapshot, error) {
	if err := c.getLocalMeta(); err != nil {
		return nil, err
	}
	rawS, ok := c.localMeta["snapshot.json"]
	if !ok {
		return nil, ErrNoLocalSnapshot
	}

	snapshot := &data.Snapshot{}
	if err := c.db.Unmarshal(rawS, snapshot, "snapshot", c.snapshotVer); err != nil {
		return nil, ErrDecodeFailed{"snapshot.json", err}
	}
	return snapshot, nil
}

// loadDelegatedTargets downloads, decodes, verifies and stores targets
func (c *Client) loadDelegatedTargets(snapshot *data.Snapshot, role string, db *verify.DB) (*data.Targets, error) {
	var err error
	fileName := role + ".json"
	fileMeta, ok := snapshot.Meta[fileName]
	if !ok {
		return nil, ErrRoleNotInSnapshot{role, snapshot.Version}
	}

	// 5.6.1 download target if not in the local store
	// 5.6.2 check against snapshot hash
	// 5.6.4 check against snapshot version
	raw, alreadyStored := c.localMetaFromSnapshot(fileName, fileMeta)
	if !alreadyStored {
		raw, err = c.downloadMetaFromSnapshot(fileName, fileMeta)
		if err != nil {
			return nil, err
		}
	}

	targets := &data.Targets{}
	// 5.6.3 verify signature with parent public keys
	// 5.6.5 verify that the targets is not expired
	// role "targets" is a top role verified by root keys loaded in the client db
	err = db.Unmarshal(raw, targets, role, fileMeta.Version)
	if err != nil {
		return nil, ErrDecodeFailed{fileName, err}
	}

	// 5.6.6 persist
	if !alreadyStored {
		if err := c.local.SetMeta(fileName, raw); err != nil {
			return nil, err
		}
	}
	return targets, nil
}
