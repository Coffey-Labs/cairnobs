CREATE TABLE IF NOT EXISTS logs
(
    `timestamp`  DateTime64(9, 'UTC'),
    `host`       String,
    `service`    String,
    `severity`   LowCardinality(String),
    `message`    String,
    `attributes` Map(String, String)
)
ENGINE = MergeTree
PARTITION BY toDate(timestamp)
ORDER BY (service, timestamp)
