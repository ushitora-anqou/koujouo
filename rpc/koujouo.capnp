@0xd4a1b2c3d5e6f7a8;
# koujouo controller <-> agent protocol.
using Go = import "/go.capnp";
$Go.package("rpc");
$Go.import("github.com/ushitora-anqou/koujouo/rpc");

using Stream = import "/capnp/stream.capnp";

struct File {
  # A file delivered to the agent, e.g. config.ini.
  name    @0 :Text;   # relative path under the write-data dir
  content @1 :Data;
}

struct BackupSettings {
  endpointURL     @0 :Text;  # e.g. "https://s3.<region>.backblazeb2.com"
  bucket          @1 :Text;
  accessKeyID     @2 :Text;  # from spec.backup.credentials.accessKeyId
  secretAccessKey @3 :Text;  # from spec.backup.credentials.secretAccessKey
}

struct Config {
  # The full desired state of one FactorioServer, sent on every configure.
  gameVersion        @0 :Text;
  files              @1 :List(File);  # config.ini, whitelist.json, ...
  discordWebhookURL  @2 :Text;        # empty disables notifications
  leaveTimeoutSeconds @3 :Int32;      # 0 = default (60)
  backup             @4 :BackupSettings;  # zero value disables backups
  downloadUsername   @5 :Text;        # for experimental/old builds; empty for stable
  downloadToken      @6 :Text;
}

enum Phase {
  pending     @0;  # agent up, game not started yet
  downloading @1;  # fetching the game archive
  running     @2;  # serving players
  draining    @3;  # warned players, waiting for leaveTimeout
  saving      @4;  # game is writing the save file
  suspended   @5;  # game stopped, agent idle
  failed      @6;
}

struct AgentStatus {
  phase       @0 :Phase;
  gameVersion @1 :Text;  # version actually running
  players     @2 :UInt16;
  # Version of the agent binary (the build of the controller image),
  # empty for an agent that predates the field. The controller upgrades
  # the agent when it differs from its own build.
  agentVersion @3 :Text;
}

interface Agent {
  # The state-changing methods carry the RPC token in a parameter of their
  # own instead of authenticating the connection once: the agent serves
  # several connections at a time, so a flag of the agent would let a
  # client that authenticated once open the way for every other client.
  # The token is the value of the Secret the controller creates for the
  # server (`<FactorioServer name>-agent-token`), which the agent Pod
  # mounts as a file; the agent compares the received token with the file
  # on every call.
  #
  # status takes no token, on purpose: it carries no secret (only the
  # version and the player count) and the controller polls it before it
  # knows whether the agent predates the token, so an old agent must keep
  # answering it.

  # configure applies the desired state. It is idempotent. On a game
  # version change it drains the players, stops the game, downloads the
  # new binary and starts it before returning.
  configure @0 (cfg :Config, token :Text) -> ();

  # status reports the current agent/game state. The controller polls it.
  status @1 () -> (st :AgentStatus);

  # stop gracefully stops the game (drain + save). Returns after the
  # save file has been written and the game process has exited.
  stop @2 (token :Text) -> ();

  # upgrade replaces the agent binary: the agent returns a ByteStream
  # capability, the controller pushes the new binary through it and ends
  # the stream with done(sha256). The agent verifies the sha256, then
  # execve(2)s the new binary. The Factorio child process keeps running;
  # its pid travels in the environment.
  upgrade @3 (token :Text) -> (bin :ByteStream);
}

interface ByteStream {
  # Streaming method: resolve the previous write before sending the
  # next chunk, so the RPC layer applies backpressure.
  write @0 (chunk :Data) -> Stream.StreamResult;
  done  @1 (sha256 :Data) -> ();  # 32 bytes; ends the stream
}
