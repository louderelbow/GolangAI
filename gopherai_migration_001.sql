-- ============================================================
-- DeepTalk 迁移 001：sessions 表新增 model_type（会话级模型绑定）
-- ------------------------------------------------------------
-- 背景：会话创建时绑定模型，之后不允许修改；因此模型类型必须持久化。
-- 旧的 sessions 表没有该字段，升级后必须先执行本脚本，否则会话相关
-- 查询会报 Unknown column 'model_type'。
--
-- 执行方式：
--   mysql -u root -p deeptalk < gopherai_migration_001.sql
--
-- 说明：DEFAULT '2' 会对存量会话统一回填为 RAG（2），
--       与旧代码「服务重启后所有会话按 RAG 处理」的行为保持一致。
--       若希望存量会话显示为 DeepSeek，可执行文件末尾的可选 UPDATE。
-- ============================================================

ALTER TABLE `sessions`
  ADD COLUMN `model_type` VARCHAR(8) NOT NULL DEFAULT '2'
  COMMENT '会话绑定的模型类型 1DeepSeek 2RAG 3MCP 4Ollama 5ReAct'
  AFTER `title`;

-- 可选：把存量会话统一改成 DeepSeek（按需二选一执行）
-- UPDATE `sessions` SET `model_type` = '1';
