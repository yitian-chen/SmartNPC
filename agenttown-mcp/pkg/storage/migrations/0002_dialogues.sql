-- 0002_dialogues.sql — NPC-to-NPC dialogue turns (Phase 2 Module C).
-- One row per chat_turn: speaker says content to listener within a conv_id.
-- Used by memory generation to summarize recent conversations per agent.
-- Query pattern: LoadRecentDialogues(agentID) → WHERE speaker_id = ? OR listener_id = ?
-- ordered by created_at DESC. Two indexes cover both sides of the OR.

CREATE TABLE IF NOT EXISTS agent_dialogues (
  id           BIGINT       NOT NULL AUTO_INCREMENT PRIMARY KEY,
  conv_id      VARCHAR(128) NOT NULL,
  speaker_id   VARCHAR(64)  NOT NULL,
  listener_id  VARCHAR(64)  NOT NULL,
  content      TEXT         NOT NULL,
  turn_index   INT          NOT NULL DEFAULT 0,
  is_end       TINYINT(1)   NOT NULL DEFAULT 0,
  created_at   DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP,
  INDEX idx_speaker_time  (speaker_id, created_at),
  INDEX idx_listener_time (listener_id, created_at),
  INDEX idx_conv           (conv_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
