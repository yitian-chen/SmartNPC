-- 0005_memory_architecture.sql — 长期记忆三表重建（替代旧 agent_memories /
-- agent_relationships）。对齐 docs/AgentTown_LongTermMemory_SQLite_Design.md：
--   episodic_memories   过去经历（game_time 锚定，只追加）
--   episodic_commitments 未来待办（pending → done/expired/cancelled）
--   semantic_people     对人认知（单一 affinity 0-100 + impression）
--
-- 存储引擎复用 MySQL（文档原为 SQLite，见设计文档"特殊说明"）：
--   - TEXT 主键改 VARCHAR(64)；emotion 用 DOUBLE
--   - participants_json 用 MySQL JSON 列
--   - 联想召回用 LIKE（MySQL 无 FTS5 trigram，不建 FULLTEXT）
-- 旧 agent_memories / agent_relationships 数据为测试数据，直接 DROP。

CREATE TABLE IF NOT EXISTS episodic_memories (
  id                VARCHAR(64)  NOT NULL PRIMARY KEY,
  owner_id          VARCHAR(64)  NOT NULL,
  game_time         BIGINT       NOT NULL,
  game_day          INT          NOT NULL,
  location          VARCHAR(128) NOT NULL DEFAULT '',
  category          VARCHAR(32)  NOT NULL,
  summary           TEXT         NOT NULL,
  participants_json JSON         NOT NULL,
  importance        TINYINT      NOT NULL DEFAULT 5,
  emotion           DOUBLE       NOT NULL DEFAULT 0,
  source            VARCHAR(16)  NOT NULL DEFAULT 'self',
  status            VARCHAR(16)  NOT NULL DEFAULT 'active',
  created_at        DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP,
  INDEX idx_ep_owner_time (owner_id, status, game_time),
  INDEX idx_ep_owner_day  (owner_id, game_day),
  INDEX idx_ep_owner_cat  (owner_id, category)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE IF NOT EXISTS episodic_commitments (
  id               VARCHAR(64)  NOT NULL PRIMARY KEY,
  owner_id         VARCHAR(64)  NOT NULL,
  origin_ep_id     VARCHAR(64)  NOT NULL DEFAULT '',
  kind             VARCHAR(32)  NOT NULL,
  counterpart_id   VARCHAR(64)  NOT NULL DEFAULT '',
  counterpart_name VARCHAR(64)  NOT NULL DEFAULT '',
  content          TEXT         NOT NULL,
  due_start        BIGINT       NOT NULL DEFAULT 0,
  due_end          BIGINT       NOT NULL DEFAULT 0,
  due_location     VARCHAR(128) NOT NULL DEFAULT '',
  status           VARCHAR(16)  NOT NULL DEFAULT 'pending',
  created_time     BIGINT       NOT NULL,
  updated_time     BIGINT       NOT NULL DEFAULT 0,
  INDEX idx_cm_owner_due         (owner_id, status, due_start, due_end),
  INDEX idx_cm_owner_counterpart (owner_id, status, counterpart_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE IF NOT EXISTS semantic_people (
  owner_id           VARCHAR(64) NOT NULL,
  target_id          VARCHAR(64) NOT NULL,
  target_name        VARCHAR(64) NOT NULL DEFAULT '',
  affinity           TINYINT     NOT NULL DEFAULT 50,
  impression         TEXT        NOT NULL,
  last_interact_time BIGINT      NOT NULL DEFAULT 0,
  updated_at         DATETIME    NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  PRIMARY KEY (owner_id, target_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

DROP TABLE IF EXISTS agent_memories;
DROP TABLE IF EXISTS agent_relationships;
