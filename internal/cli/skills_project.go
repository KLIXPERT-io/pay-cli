package cli

import (
	"context"
	"strings"

	"github.com/KLIXPERT-io/pay-cli/internal/cache"
	"github.com/KLIXPERT-io/pay-cli/internal/discovery"
	"github.com/KLIXPERT-io/pay-cli/internal/skills"
)

// The collectors below feed references/PROJECT.md (§14) with what an agent
// needs before its first block edit: whether GraphQL introspection works (and
// so what `pay describe` can know), the block catalog of the CACHED census,
// and the URL/preview/backup configuration. None of them sends a request of
// its own: the manifest is the one skillsProjectDoc already resolved, the
// census is read from the cache only (cachedCensus never builds), shards come
// from that discovery's cache, and the configuration is local.

// skillsGraphQLInfo copies discovery's GraphQL ladder result.
func skillsGraphQLInfo(m *discovery.Manifest) *skills.GraphQLInfo {
	if m == nil {
		return nil
	}
	g := m.Capabilities.GraphQL
	return &skills.GraphQLInfo{Mode: g.Mode, Introspection: g.Introspection, Path: g.Path}
}

// skillsBlockCatalog renders the cached block census into PROJECT.md's
// catalog: per entity with block rows, every blocks-field path pattern with
// its observed (and declared) block types, and the parent-scoped nested
// patterns (`group.blocks`). A missing census yields Cached=false, which the
// renderer turns into "run `pay blocks types` once".
func skillsBlockCatalog(ctx context.Context, rt *Runtime) *skills.BlockCatalog {
	census, ok := rt.cachedCensus()
	if !ok || census == nil || len(census.Entities()) == 0 {
		return &skills.BlockCatalog{}
	}
	cat := &skills.BlockCatalog{Cached: true}
	for _, e := range census.Entities() {
		cat.Censused++
		if cat.Oldest.IsZero() || (!e.BuiltAt.IsZero() && e.BuiltAt.Before(cat.Oldest)) {
			cat.Oldest = e.BuiltAt
		}
		if e.BlockRows == 0 {
			continue
		}
		kind := cache.KindCollection
		if e.Kind == discovery.EntityKindGlobal {
			kind = cache.KindGlobal
		}
		shard, _ := rt.Shard(ctx, e.Slug, kind)
		cat.Entities = append(cat.Entities, blockEntityInfo(e, discovery.NewBlockKnowledge(e.Kind, e.Slug, census, shard)))
	}
	return cat
}

func blockEntityInfo(e *discovery.CensusEntity, k *discovery.BlockKnowledge) skills.BlockEntityInfo {
	info := skills.BlockEntityInfo{
		Kind:        e.Kind,
		Slug:        e.Slug,
		BuiltAt:     e.BuiltAt,
		DocsScanned: e.DocsScanned,
		TotalDocs:   -1,
		Truncated:   e.Truncated,
		Error:       e.Error,
	}
	if e.TotalDocs != nil {
		info.TotalDocs = *e.TotalDocs
	}
	te := buildTypesEntity(k, "")
	for _, a := range te.Fields {
		if a == nil {
			continue
		}
		info.Fields = append(info.Fields, skills.BlockFieldInfo{
			Path:          a.Path,
			Types:         acceptedCounts(a.Types),
			Rows:          a.Rows,
			Docs:          a.Docs,
			EmptyRows:     a.EmptyRows,
			UntypedRows:   a.UntypedRows,
			Authoritative: a.Authoritative,
			Source:        a.Source,
		})
	}
	for _, t := range te.Types {
		for _, n := range t.Nested {
			if len(n.Accepts) == 0 {
				continue
			}
			info.Nested = append(info.Nested, skills.BlockNestedInfo{Pattern: n.Pattern, Types: acceptedCounts(n.Accepts)})
		}
	}
	return info
}

func acceptedCounts(types []discovery.AcceptedType) []skills.BlockTypeCount {
	out := make([]skills.BlockTypeCount, 0, len(types))
	for _, t := range types {
		out = append(out, skills.BlockTypeCount{Slug: t.Slug, Count: t.Count})
	}
	return out
}

// skillsSiteInfo is the resolved frontend-URL, preview and backup state — the
// same facts `pay doctor`'s site and backups checks report. The preview secret
// is resolved only to name its SOURCE; its value never leaves this function.
func skillsSiteInfo(rt *Runtime) *skills.SiteInfo {
	cfg := rt.Cfg
	if cfg == nil {
		return nil
	}
	si := &skills.SiteInfo{
		SiteURL:            cfg.SiteURL,
		SiteURLSource:      cfg.Sources["site_url"],
		URLPaths:           cfg.URLPaths,
		PreviewPath:        cfg.PreviewPath,
		PreviewPathSource:  cfg.Sources["preview_path"],
		PreviewNeedsSecret: strings.Contains(cfg.PreviewPath, "{secret}"),
		PreviewSecretEnv:   cfg.PreviewSecretEnv,
		BackupDir:          cfg.BackupDir,
		BackupDirSource:    cfg.Sources["backup_dir"],
		NoBackup:           cfg.NoBackup,
	}
	if prev, err := rt.PreviewSecret(); err == nil && prev != nil {
		si.PreviewSecretSource = prev.Source
	}
	return si
}
