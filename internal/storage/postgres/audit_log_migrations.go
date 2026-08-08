package postgres

// auditLogReadNoiseCleanupSQL retires the rows written by the pre-fix audit
// policy, which recorded every non-2xx management response including plain reads.
// A panel tab polling GET /update/progress — refused for operators without
// system.status.read, 502 wherever the updater sidecar is absent — wrote one row
// every two seconds; one deployment held 33,065 rows of which ~50 were real
// events, and the governance page was 656 pages of update polling.
//
// The cleanup applies the new policy to the history it produced rather than
// clearing the table: errored reads are no longer audit events and go entirely,
// while refused reads survive collapsed to the same one-row-per-five-minutes the
// middleware now enforces, so "this actor was refused on this route, repeatedly,
// during these hours" is still readable. Sensitive reads (bodies, egress,
// exports, downloads) are audited under the new policy too and are excluded.
const auditLogReadNoiseCleanupSQL = `
DELETE FROM audit_logs
 WHERE action IN ('management.get', 'management.head')
   AND result <> 'denied'
   AND (resource_type || '/' || resource_id) NOT LIKE '%content%'
   AND (resource_type || '/' || resource_id) NOT LIKE '%egress%'
   AND (resource_type || '/' || resource_id) NOT LIKE '%export%'
   AND (resource_type || '/' || resource_id) NOT LIKE '%download%';

DELETE FROM audit_logs a
 USING (
   SELECT id,
          row_number() OVER (
            PARTITION BY tenant_id, actor_kind, actor_user_id, action, resource_type, resource_id, result,
                         floor(extract(epoch FROM created_at) / 300)
            ORDER BY created_at, id
          ) AS repeat_rank
     FROM audit_logs
    WHERE action IN ('management.get', 'management.head')
      AND result = 'denied'
 ) collapsed
 WHERE a.id = collapsed.id
   AND collapsed.repeat_rank > 1;

-- Retention prunes by age; the existing indexes are all tenant/actor/action
-- prefixed and cannot serve a bare created_at range scan.
CREATE INDEX IF NOT EXISTS idx_audit_logs_created_at ON audit_logs(created_at);
`
