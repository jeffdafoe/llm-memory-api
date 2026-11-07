-- LLM-947 down. Remove shared_preload_libraries from
-- conf.d/50-llm-memory-tuning.conf too, or the server keeps collecting.
DROP EXTENSION IF EXISTS pg_stat_statements;
