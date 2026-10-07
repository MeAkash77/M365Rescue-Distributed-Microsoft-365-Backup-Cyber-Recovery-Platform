package source

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"m365rescue/internal/graph"
)

// Graph protects real Microsoft 365 tenants via Microsoft Graph (application permissions).
// Implemented: OneDrive files (drive delta) and Exchange Online inbox messages (message delta).
// SharePoint and Teams are intentionally reported as unsupported (see README roadmap).
type Graph struct{ C *graph.Client }

func (g *Graph) Workloads() []string { return []string{"exchange", "onedrive"} }

func (g *Graph) users(ctx context.Context, tid string) ([]string, error) {
	var ids []string
	_, err := g.C.Pages(ctx, tid, "/users?$select=id&$top=100", func(v []json.RawMessage) error {
		for _, raw := range v {
			var u struct {
				ID string `json:"id"`
			}
			if json.Unmarshal(raw, &u) == nil && u.ID != "" {
				ids = append(ids, u.ID)
			}
		}
		return nil
	})
	return ids, err
}

func (g *Graph) Delta(ctx context.Context, t Tenant, workload, token string, fn func(Item) error) (string, error) {
	if workload != "onedrive" && workload != "exchange" {
		return "", ErrUnsupported
	}
	state := map[string]string{} // per-user deltaLink
	if token != "" {
		_ = json.Unmarshal([]byte(token), &state)
	}
	uids, err := g.users(ctx, t.MSTenantID)
	if err != nil {
		return "", err
	}
	for _, uid := range uids {
		start := state[uid]
		if start == "" {
			if workload == "onedrive" {
				start = fmt.Sprintf("/users/%s/drive/root/delta", uid)
			} else {
				start = fmt.Sprintf("/users/%s/mailFolders/inbox/messages/delta?$select=subject,lastModifiedDateTime", uid)
			}
		}
		link, err := g.C.Pages(ctx, t.MSTenantID, start, func(vals []json.RawMessage) error {
			for _, raw := range vals {
				if it, ok := parseItem(workload, uid, raw); ok {
					if err := fn(it); err != nil {
						return err
					}
				}
			}
			return nil
		})
		if err != nil {
			var he *graph.HTTPError
			if errors.As(err, &he) && he.Code == 404 { // user without OneDrive / mailbox
				continue
			}
			return "", err
		}
		if link != "" {
			state[uid] = link
		}
	}
	b, _ := json.Marshal(state)
	return string(b), nil
}

func parseItem(workload, uid string, raw json.RawMessage) (Item, bool) {
	var v struct {
		ID      string    `json:"id"`
		Name    string    `json:"name"`
		Subject string    `json:"subject"`
		Mod     string    `json:"lastModifiedDateTime"`
		Folder  *struct{} `json:"folder"`
		Deleted *struct{} `json:"deleted"`
		Removed *struct{} `json:"@removed"`
		Parent  struct {
			Path string `json:"path"`
		} `json:"parentReference"`
	}
	if json.Unmarshal(raw, &v) != nil || v.ID == "" {
		return Item{}, false
	}
	it := Item{ID: uid + ":" + v.ID, Mod: v.Mod, Meta: map[string]string{"uid": uid, "id": v.ID}}
	if v.Deleted != nil || v.Removed != nil {
		it.Deleted = true
		return it, true
	}
	if workload == "onedrive" {
		if v.Folder != nil {
			return Item{}, false
		}
		parent := ""
		if i := strings.Index(v.Parent.Path, "root:"); i >= 0 {
			parent = v.Parent.Path[i+5:]
		}
		it.Name = v.Name
		it.Meta["path"] = "/" + strings.Trim(parent, "/") + "/" + v.Name
	} else {
		it.Name = v.Subject
	}
	return it, true
}

func (g *Graph) Fetch(ctx context.Context, t Tenant, workload string, it Item) ([]byte, error) {
	uid, id := it.Meta["uid"], it.Meta["id"]
	if workload == "onedrive" {
		return g.C.Get(ctx, t.MSTenantID, fmt.Sprintf("/users/%s/drive/items/%s/content", uid, id))
	}
	return g.C.Get(ctx, t.MSTenantID, fmt.Sprintf("/users/%s/messages/%s?$expand=attachments", uid, id))
}

var readOnlyMsgFields = []string{"id", "@odata.etag", "@odata.context", "parentFolderId", "conversationId", "conversationIndex",
	"changeKey", "createdDateTime", "lastModifiedDateTime", "receivedDateTime", "sentDateTime", "webLink", "internetMessageId", "isDraft"}

// Restore (best effort, not exercised against a live tenant in this repo's tests):
// OneDrive -> PUT content to the original path (<4 MB simple upload); Exchange -> re-create message in Inbox.
func (g *Graph) Restore(ctx context.Context, t Tenant, workload string, it Item, data []byte) error {
	uid := it.Meta["uid"]
	if workload == "onedrive" {
		parts := strings.Split(it.Meta["path"], "/")
		for i := range parts {
			parts[i] = url.PathEscape(parts[i])
		}
		_, err := g.C.Do(ctx, t.MSTenantID, "PUT", fmt.Sprintf("/users/%s/drive/root:%s:/content", uid, strings.Join(parts, "/")), data, "application/octet-stream")
		return err
	}
	var msg map[string]any
	if err := json.Unmarshal(data, &msg); err != nil {
		return err
	}
	for _, k := range readOnlyMsgFields {
		delete(msg, k)
	}
	if atts, ok := msg["attachments"].([]any); ok {
		for _, a := range atts {
			if m, ok := a.(map[string]any); ok {
				delete(m, "id")
				delete(m, "lastModifiedDateTime")
			}
		}
	}
	body, _ := json.Marshal(msg)
	_, err := g.C.Do(ctx, t.MSTenantID, "POST", fmt.Sprintf("/users/%s/mailFolders/inbox/messages", uid), body, "application/json")
	return err
}
