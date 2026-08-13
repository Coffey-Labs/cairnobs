ALTER TABLE logs
    ADD COLUMN record_id UUID DEFAULT generateUUIDv4(),
    ADD INDEX record_id_idx record_id TYPE bloom_filter GRANULARITY 4
