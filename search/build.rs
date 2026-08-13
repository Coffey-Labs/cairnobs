fn main() -> Result<(), Box<dyn std::error::Error>> {
    // logs.proto is compiled only for the LogRecord message type (to
    // decode what's read off Redpanda) -- this service never calls or
    // implements LogIngest. search.proto is compiled for the
    // SearchService server this binary implements.
    tonic_build::configure().compile_protos(
        &[
            "../proto/sentry/logs/v1/logs.proto",
            "../proto/sentry/search/v1/search.proto",
        ],
        &["../proto"],
    )?;
    Ok(())
}
