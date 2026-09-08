package cli

import (
	"encoding/json"
	"time"
)

// These admin endpoints return their complete lists without pagination. Filter
// only the CLI view so older servers work and REST history stays available.
func (r *runner) listActiveAdmin(inv *invocation) error {
	req, err := r.request(inv, inv.cmd.route)
	if err != nil {
		return err
	}
	resp, err := r.client.Do(r.ctx, *req)
	if err != nil {
		return err
	}
	if !inv.boolean("--all") {
		resp.Data, err = filterAdminList(resp.Data, inv.cmd.route, inv.str("--status"), r.env.Now())
		if err != nil {
			return err
		}
	}
	return r.out.Emit(resp)
}

func filterAdminList(data json.RawMessage, route, status string, now time.Time) (json.RawMessage, error) {
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(data, &envelope); err != nil {
		return nil, err
	}
	var rows []json.RawMessage
	if err := json.Unmarshal(envelope["items"], &rows); err != nil {
		return nil, err
	}
	visible := make([]json.RawMessage, 0, len(rows))
	for _, raw := range rows {
		var row struct {
			ExpiresAt   *string `json:"expires_at"`
			ConsumedAt  *string `json:"consumed_at"`
			RevokedAt   *string `json:"revoked_at"`
			ActivatedAt *string `json:"activated_at"`
			Status      string  `json:"status"`
		}
		if err := json.Unmarshal(raw, &row); err != nil {
			return nil, err
		}
		expired := false
		if row.ExpiresAt != nil {
			expiresAt, err := time.Parse(time.RFC3339Nano, *row.ExpiresAt)
			if err != nil {
				return nil, err
			}
			expired = !now.Before(expiresAt)
		}
		if route == "admin_clients_list" && row.ActivatedAt != nil {
			expired = false
		}
		if expired || row.RevokedAt != nil {
			continue
		}
		if route == "admin_enrollment_codes_list" && row.ConsumedAt != nil {
			continue
		}
		if route == "admin_authorization_requests_list" && status == "" && row.Status != "pending" && row.Status != "approved" {
			continue
		}
		visible = append(visible, raw)
	}
	items, err := json.Marshal(visible)
	if err != nil {
		return nil, err
	}
	envelope["items"] = items
	return json.Marshal(envelope)
}
