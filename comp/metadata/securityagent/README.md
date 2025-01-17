# Security-agent metadata Payload

This package populates some of the security-agent related fields in the `inventories` product in DataDog. More specifically the
`security-agent` table.

This is enabled by default but can be turned off using `inventories_enabled` config.

The payload is sent every 10min (see `inventories_max_interval` in the config).

## Security-agent Configuration

Security-agent configurations are scrubbed from any sensitive information (same logic than for the flare).
This include the following:
`full_configuration`
`provided_configuration`
`file_configuration`
`environment_variable_configuration`
`agent_runtime_configuration`
`remote_configuration`
`cli_configuration`
`source_local_configuration`

Sending Security-Agent configuration can be disabled using `inventories_configuration_enabled`.

# Format

The payload is a JSON dict with the following fields

- `hostname` - **string**: the hostname of the agent as shown on the status page.
- `timestamp` - **int**: the timestamp when the payload was created.
- `security_agent_metadata` - **dict of string to JSON type**:
  - `agent_version` - **string**: the version of the Agent sending this payload.
  - `full_configuration` - **string**: the current Security-Agent configuration scrubbed, including all the defaults, as a YAML
    string.
  - `provided_configuration` - **string**: the current Security-Agent configuration (scrubbed), without the defaults, as a YAML
    string. This includes the settings configured by the user (throuh the configuration file, the environment, CLI...).
  - `file_configuration` - **string**: the Security-Agent configuration specified by the configuration file (scrubbed), as a YAML string.
    Only the settings written in the configuration file are included, and their value might not match what's applyed by the agent since they can be overriden by other sources.
  - `environment_variable_configuration` - **string**: the Security-Agent configuration specified by the environment variables (scrubbed), as a YAML string.
    Only the settings written in the environment variables are included, and their value might not match what's applyed by the agent somce they can be overriden by other sources.
  - `agent_runtime_configuration` - **string**: the Security-Agent configuration set by the agent itself (scrubbed), as a YAML string.
    Only the settings set by the agent itself are included, and their value might not match what's applyed by the agent since they can be overriden by other sources.
  - `remote_configuration` - **string**: the Security-Agent configuration specified by the Remote Configuration (scrubbed), as a YAML string.
    Only the settings currently used by Remote Configuration are included, and their value might not match what's applyed by the agent since they can be overriden by other sources.
  - `cli_configuration` - **string**: the Security-Agent configuration specified by the CLI (scrubbed), as a YAML string.
    Only the settings set in the CLI are included.
  - `source_local_configuration` - **string**: the Security-Agent configuration synchronized from the local Agent process, as a YAML string.

("scrubbed" indicates that secrets are removed from the field value just as they are in logs)

## Example Payload

Here an example of an inventory payload:

```
{
    "security_agent_metadata": {
        "agent_version": "7.55.0",
        "full_configuration": "<entire yaml configuration for security-agent>",
        "provided_configuration": "runtime_security_config:\n  socket: /opt/datadog-agent/run/runtime-security.sock",
        "file_configuration": "runtime_security_config:\n  socket: /opt/datadog-agent/run/runtime-security.sock",
        "environment_variable_configuration": "{}",
        "remote_configuration": "{}",
        "cli_configuration": "{}"
    }
    "hostname": "my-host",
    "timestamp": 1631281754507358895
}
```
