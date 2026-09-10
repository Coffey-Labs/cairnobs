fn main() -> Result<(), Box<dyn std::error::Error>> {
    tonic_prost_build::configure()
        .build_server(false)
        .compile_protos(
            &[
                "../../proto/sentry/logs/v1/logs.proto",
                "../../proto/sentry/agent/v1/agent_control.proto",
            ],
            &["../../proto"],
        )?;
    Ok(())
}
