-- Agent lifecycle commands (Phase: agent management punch-list item 1,
-- see /docs/agent-management-design.md). A one-shot action, not a
-- persistent desired state like desired_override -- pending_command is
-- cleared atomically by ingest/internal/agentregistry.Registry.CheckIn
-- the moment it's handed to the agent in a response, not once the agent
-- confirms execution (a restarting agent's process is gone before it
-- could send that confirmation). issued_at/issued_by are kept even
-- after the command clears, as the last-issued record for the web UI
-- and as a lightweight trail alongside the real audit_log entry
-- (event_type = 'agent_command', see enterprise/internal/audit).
ALTER TABLE agents
    ADD COLUMN pending_command TEXT,
    ADD COLUMN command_issued_at TIMESTAMPTZ,
    ADD COLUMN command_issued_by TEXT;

ALTER TABLE agents
    ADD CONSTRAINT agents_pending_command_check
    CHECK (pending_command IS NULL OR pending_command IN ('restart'));
