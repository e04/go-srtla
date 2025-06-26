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
./go-srtla -srtla_port 5000 -srt_hostname 127.0.0.1 -srt_por 5001
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
- `-version`: Show version number
- `-help`: Show help

## License

GNU Affero General Public License v3.0 (AGPL-3.0)
