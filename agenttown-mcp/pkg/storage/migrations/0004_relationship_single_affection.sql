-- 0004_relationship_single_affection.sql — 对人认知收敛为单一好感度。
--
-- 移除 familiarity（熟悉度）与 interaction_count（互动计数）两列，只保留
-- affection（0-10 好感度，默认 5 中性）。单一数值维度符合"对人认知用一个
-- 好感度值确定"的简化（见 docs/AgentTown_LongTermMemory_SQLite_Design.md）。
-- 熟悉度/互动计数从未被业务读路径之外使用（仅关系段展示），可安全丢弃。

ALTER TABLE agent_relationships DROP COLUMN familiarity;
ALTER TABLE agent_relationships DROP COLUMN interaction_count;
