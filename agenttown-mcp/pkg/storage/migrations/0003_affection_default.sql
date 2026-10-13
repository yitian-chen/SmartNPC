-- 0003_affection_default.sql — 好感度（affection）默认值 5 与历史回填。
--
-- affection 语义：每个 NPC 对其他 NPC 的 0-10 好感度，默认（尚未评估）为 5。
-- 旧代码从未更新过 affection（SaveRelationship 一直传 delta=0），历史行全是 0。
-- 本迁移把列默认值改为 5（新建关系默认 5），并把历史 affection=0 的行回填为 5。

ALTER TABLE agent_relationships MODIFY COLUMN affection TINYINT NOT NULL DEFAULT 5;

UPDATE agent_relationships SET affection = 5 WHERE affection = 0;
