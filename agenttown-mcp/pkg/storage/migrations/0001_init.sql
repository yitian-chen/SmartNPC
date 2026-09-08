-- 0001_init.sql — Stage 3 initial schema.
-- Tables: schema_migrations + agent_schedule_state (Stage 3) +
-- agent_memories / agent_relationships / action_history (pre-stubbed for Stage 4/5).

CREATE TABLE IF NOT EXISTS schema_migrations (
  version    VARCHAR(64)  NOT NULL PRIMARY KEY,
  applied_at DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP
);

-- Stage 3: per-agent schedule state (the 4 persistent fields).
-- One row per agent, upserted on every write-through.
CREATE TABLE IF NOT EXISTS agent_schedule_state (
  agent_id           VARCHAR(64) NOT NULL PRIMARY KEY,
  daily_plan         TEXT        NOT NULL,
  current_day        INT         NOT NULL DEFAULT -1,
  current_plan_index INT         NOT NULL DEFAULT 0,
  current_slot       VARCHAR(64) NOT NULL DEFAULT '',
  updated_at         DATETIME    NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

-- Stage 4 pre-stub: memory store. Go CRUD API lands in Stage 4.
CREATE TABLE IF NOT EXISTS agent_memories (
  id                BIGINT      NOT NULL AUTO_INCREMENT PRIMARY KEY,
  agent_id          VARCHAR(64) NOT NULL,
  memory_type       VARCHAR(32) NOT NULL,
  content           TEXT        NOT NULL,
  importance        TINYINT     NOT NULL DEFAULT 50,
  related_agent_id  VARCHAR(64) DEFAULT NULL,
  related_object_id VARCHAR(64) DEFAULT NULL,
  related_zone_id   VARCHAR(64) DEFAULT NULL,
  created_at        DATETIME    NOT NULL DEFAULT CURRENT_TIMESTAMP,
  last_accessed_at  DATETIME    NOT NULL DEFAULT CURRENT_TIMESTAMP,
  decay_score       FLOAT       NOT NULL DEFAULT 1.0,
  INDEX idx_agent_type    (agent_id, memory_type),
  INDEX idx_agent_created (agent_id, created_at),
  INDEX idx_agent_decay   (agent_id, decay_score)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

-- Stage 5 pre-stub: pairwise agent relationships. Go CRUD API lands in Stage 5.
CREATE TABLE IF NOT EXISTS agent_relationships (
  agent_a             VARCHAR(64) NOT NULL,
  agent_b             VARCHAR(64) NOT NULL,
  familiarity         TINYINT     NOT NULL DEFAULT 0,
  affection           TINYINT     NOT NULL DEFAULT 0,
  interaction_count   INT         NOT NULL DEFAULT 0,
  last_interaction_at DATETIME    DEFAULT NULL,
  updated_at          DATETIME    NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  PRIMARY KEY (agent_a, agent_b),
  INDEX idx_agent_b (agent_b)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

-- Stage 4 pre-stub: executed-action history (for memory consolidation + audit).
CREATE TABLE IF NOT EXISTS action_history (
  id           BIGINT      NOT NULL AUTO_INCREMENT PRIMARY KEY,
  agent_id     VARCHAR(64) NOT NULL,
  action_id    VARCHAR(128) DEFAULT NULL,
  cmd          VARCHAR(64) NOT NULL,
  params       JSON        DEFAULT NULL,
  source       VARCHAR(32) NOT NULL DEFAULT '',
  started_at   DATETIME    NOT NULL DEFAULT CURRENT_TIMESTAMP,
  completed_at DATETIME    DEFAULT NULL,
  result       VARCHAR(32) DEFAULT NULL,
  duration_ms  INT         DEFAULT NULL,
  INDEX idx_agent_time (agent_id, started_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
