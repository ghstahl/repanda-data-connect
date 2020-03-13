---
title: statsd
type: metrics
---

<!--
     THIS FILE IS AUTOGENERATED!

     To make changes please edit the contents of:
     lib/metrics/statsd.go
-->


Pushes metrics using the [StatsD protocol](https://github.com/statsd/statsd).
Supported tagging formats are 'legacy', 'none', 'datadog' and 'influxdb'.

```yaml
# Config fields, showing default values
metrics:
  statsd:
    prefix: benthos
    address: localhost:4040
    flush_period: 100ms
    tag_format: legacy
```

The underlying client library has recently been updated in order to support
tagging. The tag format 'legacy' is default and causes Benthos to continue using
the old library in order to preserve backwards compatibility.

The legacy library aggregated timing metrics, so dashboards and alerts may need
to be updated when migrating to the new library.

The 'network' field is deprecated and scheduled for removal. If you currently
rely on sending Statsd metrics over TCP and want it to be supported long term
please [raise an issue](https://github.com/Jeffail/benthos/issues).

## Fields

### `prefix`

A string prefix to add to all metrics.


Type: `string`  
Default: `"benthos"`  

### `address`

The address to send metrics to.


Type: `string`  
Default: `"localhost:4040"`  

### `flush_period`

The time interval between metrics flushes.


Type: `string`  
Default: `"100ms"`  

### `tag_format`

Metrics tagging is supported in a variety of formats. The format 'legacy' is a special case that forces Benthos to use a deprecated library for backwards compatibility.


Type: `string`  
Default: `"legacy"`  
Options: `none`, `datadog`, `influxdb`, `legacy`.

