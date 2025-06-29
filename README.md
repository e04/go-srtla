# go-srtla

A Go implementation of [SRTLA](https://github.com/irlserver/srtla) receiver that proxies SRT transport with link aggregation capabilities.

Supports cross-platform environment.

## Usage

### Pre-built Binaries

Download the latest pre-built binaries from the [Releases](https://github.com/e04/go-srtla/releases) page

### From Source

#### Build

```bash
go build -o go-srtla
```

After building, run the binary:

```bash
./go-srtla [options]
```

For example:

```bash
./go-srtla -srtla_port 5000 -srt_hostname 127.0.0.1 -srt_port 5001
```

### Receiving in OBS

To receive the aggregated SRT stream in OBS:

1. Add a "Media Source" in OBS
2. Set the input URL to: `srt://127.0.0.1:5001/?mode=listener`
3. Set the input format to: `mpegts`

### Options

- `-srtla_port <port>`: Port to bind the SRTLA socket to (default: 5000)
- `-srt_hostname <hostname>`: Hostname of the downstream SRT server (default: 127.0.0.1)
- `-srt_port <port>`: Port of the downstream SRT server (default: 5001)
- `-verbose`: Enable verbose logging
- `-help`: Show help

## Related Tools

You can build a more advanced and resilient streaming workflow by combining `go-srtla` with these related tools:

- **[srt-live-reporter](https://github.com/e04/srt-live-reporter)**  
  A proxy that can be placed downstream from `go-srtla` to provide statistics (latency, packet loss, etc.) for the aggregated SRT stream via WebSocket. This is useful for real-time monitoring of your stream's health.

- **[obs-srt-bridge](https://github.com/e04/obs-srt-bridge)**  
  Works in conjunction with `srt-live-reporter` to automatically switch OBS scenes based on SRT statistics. For example, you can configure it to automatically switch to a "Technical Difficulties" scene if network quality degrades, creating a more professional and automated broadcast.

## Usage Example

- **[Building a IRL Streaming Setup with go-srtla, srt-live-reporter and obs-srt-bridge](https://gist.github.com/e04/3914d98d6d0a55c689ab724ac6896081)**  
  This Gist provides a detailed, step-by-step guide on how to set up a full bonding workflow: streaming from a smartphone app over multiple connections (e.g., Cellular and Wi-Fi), receiving it with `go-srtla`, and finally pulling it into OBS.

## License

GNU Affero General Public License v3.0 (AGPL-3.0)
