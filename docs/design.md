# koujouo 実装設計書

実装前の設計調査成果物。正本は `docs/factorio-server-plan.html`、開発規約は `AGENTS.md`。
本書では (1) ライブラリ選定、(2) Nix/ビルド統合、(3) CRD スキーマ最終案、(4) capnp スキーマ案、(5) ファイルレイアウト、(6) テスト計画を定める。
認証情報の実値は本書に掲載しない（プレースホルダーのみ使用する）。

前提の再確認:

- controller イメージに agent バイナリを同梱する（CNPG 方式）。`spec.agent.image` は存在しない。
- agent は PID 1 として `/kojouo/agent` を実行する。tini 不要（agent が子プロセスを回収する）。
- controller ↔ agent は Cap'n Proto RPC over plain TCP（TLS なし、pod IP 直、port 9550）。
- ゲームバイナリは agent が `https://factorio.com/get-download/{version}/headless/linux64` から generic ephemeral volume へダウンロードする。
- セーブは既存 PVC、Service はユーザー側 Helm chart が用意する。
- API group は `koujouo.anqou.net` に修正する（現状 `koujouo.anqou.net.koujouo.anqou.net`）。
- モジュールパスは `github.com/ushitora-anqou/koujouo`、Go 1.26.0。

---

## 1. ライブラリ選定

### 1.1 Cap'n Proto RPC — `capnproto.org/go/capnp/v3` @ v3.1.0-alpha.2

| 項目 | 内容 |
|---|---|
| module path | `capnproto.org/go/capnp/v3` |
| バージョン | v3.1.0-alpha.2（2025-10-24 tag。2026-07 時点でも main へのコミットが続く公式後継） |
| リポジトリ | github.com/capnproto/go-capnp（stars 約 1400） |
| ライセンス | MIT |
| メンテ状況 | Cap'n Proto 公式 org 配下で開発継続。v3 系はまだ alpha タグだが、v1/v2 からの事実上の移行先であり代替がない（§8 懸念参照） |

**コード生成**。スキーマに Go 用アノテーションが必須:

```capnp
using Go = import "/go.capnp";
@0x...;
$Go.package("rpc");
$Go.import("github.com/ushitora-anqou/koujouo/rpc");
```

コンパイルコマンド（`-I` にモジュール内 `std/` を指定するのがポイント）:

```bash
capnp compile -I"$(go list -m -f '{{.Dir}}' capnproto.org/go/capnp/v3)/std" -ogo rpc/koujouo.capnp
```

`capnp` コンパイラは `-ogo` で `capnpc-go` プラグインを呼び出す。実装では CodeGeneratorRequest を `-o-` で標準出力に流し、go.mod の `tool` ディレクティブが指す `go tool capnpc-go` に渡す（§2.1）。

**RPC サーバー（agent 側、TCP 9550）**:

```go
lis, err := net.Listen("tcp", ":9550")
boot := capnp.Client(rpcapi.Agent_ServerToClient(agentImpl))
err = rpc.Serve(lis, boot) // 接続ごとに Conn を張り、ブロックする
```

**RPC クライアント（controller 側）**:

```go
conn, err := net.Dial("tcp", net.JoinHostPort(pod.Status.PodIP, "9550"))
c := rpc.NewConn(rpc.NewStreamTransport(conn), nil) // Options は nil でよい
defer c.Close()
a := rpcapi.Agent(c.Bootstrap(ctx))
f, release := a.Configure(ctx, func(ps rpcapi.Agent_configure_Params) error {
	return setConfig(ps.Cfg(), cfg)
})
defer release()
_, err = f.Struct() // 戻り値の解決を待つ
```

**ストリーミング（agent バイナリ送信）**。capability は**戻り値**で受け取る。`write` は呼び出し側から server へデータを送るメソッドなので、引数で capability を渡しても agent 側は受け取れない（実装時に検証）。そのため `upgrade @3 () -> (bin :ByteStream);` とし、controller が戻り値の capability に書き込む:

```go
f, release := a.Upgrade(ctx)
result, err := f.Struct()
bin := rpcapi.ByteStream(result.Bin())   // controller 側はクライアント
w, release := bin.Write(ctx, func(ps rpcapi.ByteStream_write_Params) error {
	return ps.SetChunk(chunk)
})
_ = w.WaitStreaming()                    // 前の write が解決してから次を送る
d, release := bin.Done(ctx, func(ps rpcapi.ByteStream_done_Params) error {
	return ps.SetSha256(sum)
})
```

`write @0 (chunk :Data) -> stream;` のように戻り値型 `stream`（`std/capnp/stream.capnp` の `StreamResult`）にするとストリーミングメソッドになり、「前回の write の promise が解決してから次の write を送る」でバックプレッシャーが得られる。

agent は sha256 検証後に `execve(2)` するため、`done` の応答は接続切断で失敗する。controller は `capnp exc.IsType(err, exc.Disconnected)` を成功とみなし、再 dial して `agentVersion` が更新されたことで完了を判定する。

選定理由: プランの決定事項。双方向 capability 渡しが必要な upgrade ユースケースに合う。

### 1.2 xz 復号 — `github.com/ulikunitz/xz` @ v0.5.17

| 項目 | 内容 |
|---|---|
| module path | `github.com/ulikunitz/xz` |
| バージョン | v0.5.17（2026-09-19 リリース） |
| メンテ状況 | 純 Go、依存なし、コミット継続中。CVE-2025-58058（破損 LZMA によるメモリリーク DoS）は v0.5.14 で修正済みであり v0.5.17 は影響外 |

`klauspost/compress` との比較: klauspost/compress は zstd/s2/gzip/flate/zip/huff0/xpress を扱うが **xz 形式を含まない**。Factorio の配布物は `tar.xz` のため ulikunitz/xz 一択。

使い方（`archive/tar` と組み合わせる）:

```go
xr, err := xz.NewReader(resp.Body)
if err != nil { return err }
tr := tar.NewReader(xr)
for {
	hdr, err := tr.Next()
	if err == io.EOF { break }
	if err != nil { return err }
	// hdr.Name を dest 配下に安全に展開（パストラバーサ防止のため filepath.Clean で検証）
}
```

### 1.3 RCON クライアント — `github.com/gorcon/rcon` @ v1.4.0

| 項目 | 内容 |
|---|---|
| module path | `github.com/gorcon/rcon` |
| バージョン | v1.4.0（2024-11 タグ）。2026-06 にも認証パケット修正のコミットあり |
| ライセンス | MIT |
| メンテ状況 | 低頻度だが存続。Source RCON プロトコルの完全実装で Factorio で動作する |

```go
conn, err := rcon.Dial("127.0.0.1:55555", password) // pod 内のみ
if err != nil { return err }
defer conn.Close()
resp, err := conn.Execute("/players online count")
```

自前実装へのフォールバック案（メンテ停止時）: RCON パケットは `int32 length + int32 id + int32 type + body + NUL 2 つ` の固定形式で、認証 (type 3) とコマンド (type 2) を往復するだけ。`internal/agent/rcon.go` のラッパー以下に閉じ込めておけば、置換は約 100 行で済む。

### 1.4 S3 アップロード — `github.com/aws/aws-sdk-go-v2`（config / credentials / service/s3）

| 項目 | 内容 |
|---|---|
| module path | `github.com/aws/aws-sdk-go-v2/config`、`github.com/aws/aws-sdk-go-v2/credentials`、`github.com/aws/aws-sdk-go-v2/service/s3`（いずれも `go get ...@latest` で v1.x 系） |
| メンテ状況 | AWS 公式。モジュール分割されており S3 のために必要な部品だけを依存に追加できる |

`minio-go` との比較: minio-go は単一 large モジュールで S3 互換に特化しており記述は短いが、API surface が大きく依存が重い。Backblaze B2 は AWS SigV4 互換を公式に掲げており、aws-sdk-go-v2 + カスタム endpoint + path-style での運用実績が豊富である。本 operator の用途は「既知キーへの PutObject のみ（削除・一覧なし）」で最小限のため、aws-sdk-go-v2 を選ぶ。

B2 対応の構成（`BaseEndpoint` + `UsePathStyle` + static credentials）:

```go
cfg, err := config.LoadDefaultConfig(ctx,
	config.WithCredentialsProvider(
		credentials.NewStaticCredentialsProvider(accessKey, secretKey, "")),
	config.WithRegion("us-west-00"), // B2 はリージョン名が必要（spec で指定）
)
client := s3.NewFromConfig(cfg, func(o *s3.Options) {
	o.BaseEndpoint = aws.String(endpointURL) // 例: https://s3.<region>.backblazeb2.com
	o.UsePathStyle = true                     // B2 は path-style 必須
})
_, err = client.PutObject(ctx, &s3.PutObjectInput{
	Bucket: aws.String(bucket),
	Key:    aws.String(key), // factorio-map-YYYYMMDD-HHMMSS.zip
	Body:   file,
})
```

セーブファイルは数十 MB 程度のため `PutObject` で十分。`feature/s3/manager`（マルチパート Uploader）は不要。

### 1.5 Discord webhook — 標準 `net/http`（ライブラリ追加なし）

Discord の Execute Webhook API は `POST {webhook URL}` に `{"content": "...", "username": "Factorio"}` という JSON を `Content-Type: application/json` で送るだけ（公式ドキュメント確認済み）。discordgo 等のボット用ライブラリは過剰。429/5xx 時のリトライ（3 回、指数バックオフ）のみ自前実装する。

### 1.6 依存追加まとめ

| 用途 | module | バージョン |
|---|---|---|
| RPC | `capnproto.org/go/capnp/v3` | v3.1.0-alpha.2 |
| xz | `github.com/ulikunitz/xz` | v0.5.17 |
| RCON | `github.com/gorcon/rcon` | v1.4.0 |
| S3 | `github.com/aws/aws-sdk-go-v2/{config,credentials,service/s3}` | latest (v1.x) |
| Discord | なし（`net/http`） | — |
| CLI / 設定 | `github.com/spf13/cobra` / `github.com/spf13/viper` | latest (v1.x) |

---

## 2. Nix / ビルド統合

### 2.1 devShell へのツール追加

`capnp` コンパイラは nixpkgs に存在する（`pkgs/by-name/ca/capnproto/package.nix` → `capnproto`）ので、そのまま devShell に追加する。Go プラグイン `capnpc-go` は flake ではビルドしない: `go.mod` の `tool` ディレクティブ（`capnproto.org/go/capnp/v3/capnpc-go`、`capnproto.org/go/capnp/v3` と同じバージョン）で管理し、`rpc/generate.go` の `go:generate` が `go tool capnpc-go` として実行する。バージョンは `go get -tool capnproto.org/go/capnp/v3/capnpc-go@<version>` で上げる。

当初は `capnpc-go` を flake 内の `buildGoModule` として定義する計画だったが、`tool` ディレクティブで版を 1 か所（go.mod）に寄せられるため取りやめた。`go tool` は go.mod が固定した版をモジュールキャッシュから実行するだけでインストールはしないため、AGENTS.md の「ツールは `go install` せず Nix から」という規約とも両立する。

### 2.2 Makefile ターゲットと go:generate

`rpc/generate.go` に `go:generate` ディレクティブを置く:

```go
// Package rpc contains the generated Cap'n Proto protocol for the agent.
package rpc

//go:generate capnp compile -I`go list -m -f {{.Dir}} capnproto.org/go/capnp/v3`/std -ogo koujouo.capnp
```

Makefile に追加し、`generate` ターゲットから呼ぶ:

```make
.PHONY: generate-capnp
generate-capnp: ## Generate Go code from rpc/koujouo.capnp.
	go generate ./rpc

.PHONY: generate
generate: generate-capnp ## Generate code containing DeepCopy, ...
	"$(CONTROLLER_GEN)" object:headerFile="hack/boilerplate.go.txt",year=$(YEAR) paths="./..."
```

`capnp` / `capnpc-go` は §2.1 のとおり devShell 由来とし、Makefile 末尾のツール変数に `CAPNP ?= capnp` を追加しない（go:generate 内で PATH 解決する）。

`make build` / `make run` は `cmd/main.go` から `cmd/manager/main.go` へパスを更新する。`make run` は `--config examples/manager.yaml` を渡す（§2.5）。

### 2.3 flake のサブパッケージ化（cmd/manager と cmd/agent）

`cmd/main.go` を `cmd/manager/main.go` へ移動し、`cmd/agent/main.go` を新設する。flake の `manager` パッケージは [go2nix](https://github.com/numtide/go2nix) の `buildGoApplication` でビルドする:

```nix
goEnv = go2nix.lib.mkGoEnv {
  inherit (pkgs) go callPackage;
  go2nix = go2nix.packages.${system}.go2nix;
  # scope の標準ライブラリも cgo 無しでビルドする（下記）
  goEnv = { CGO_ENABLED = "0"; };
};

manager = goEnv.buildGoApplication {
  pname = "koujouo";
  version = "0.0.1";
  src = inputs.self;
  subPackages = [ "cmd/manager" "cmd/agent" ]; # 両バイナリを 1 呼び出しで
  CGO_ENABLED = 0;                             # 静的バイナリを維持
  doCheck = false;                             # envtest は sandbox で動かない（make test が担当）
  ldflags = [ "-s" "-w" "-X github.com/ushitora-anqou/koujouo/internal/version.Agent=${buildVersion}" ];
}; # バイナリ名がディレクトリ名 (manager, agent) になる
```

`subPackages` の各エントリは `$out/bin/<ディレクトリ名>` になるため、`$out/bin/manager` と `$out/bin/agent` の両方が生成される。`src` が flake のソース（git-tracked のみ）である点は `buildGoModule` のときと同じ。

`CGO_ENABLED` は 2 か所に要る。ビルド側だけ `0` にすると、scope が既定（cgo 有効）でビルドした標準ライブラリが `runtime/cgo` を同梱し、リンカが外部リンカを選んでバイナリが Nix の glibc に動的リンクされる。agent のバイナリは別 image の agent Pod に流し込まれる（in-place upgrade）ため、image 間で glibc の store path が変わると `execve` が失敗する。scope 側も `0` にして静的リンクを保つ。

**lockfile を持たない**: `goLock` を渡さないので、モジュールのハッシュは評価時に `go.sum` から導出される（結果は `$XDG_CACHE_HOME/go2nix/nar/` にキャッシュされる）。`go.mod` / `go.sum` だけを更新する PR（Dependabot）がそのままビルドでき、`vendorHash` のような手動更新は不要になる。`go2nix.toml` を置くと `go get` のたびに再生成が必要で、忘れるとビルドが落ちるため採用しない。

**Nix プラグインが必要**: lockfile 無しのビルドは、評価時に `builtins.resolveGoPackages`（go2nix の Nix プラグインが提供）を呼んで `go list` を回す。プラグインは Nix 2.34 でビルドされ、読み込む Nix と ABI が一致している必要がある。

- flake は `packages.<system>.go2nix-nix-plugin` として公開する（Makefile と CI がパスを取得する）
- devShell の `shellHook` は `NIX_CONFIG` に `plugin-files = <plugin>` を追記する（既存の `NIX_CONFIG` は保持）。`nix develop` 内の `nix build .#manager` / `.#dockerImage` はそのまま動く
- devShell 外では `nix build --option plugin-files "$(nix build --no-link --print-out-paths .#go2nix-nix-plugin)/lib/nix/plugins/libgo2nix_plugin.so" .#manager`。`make docker-build` はこのオプションを自前で渡すので外でも動く
- プラグインが無いと評価は `error: attribute 'resolveGoPackages' missing` で止まる

評価は `go list` を実行するため、モジュールが `GOMODCACHE` に無ければ `GOPROXY` に到達する（`go mod download` を先に流しておけば良い）。

### 2.4 dockerImage に両バイナリを同梱

```nix
glibcLoader =
  if pkgs.stdenv.hostPlatform.isAarch64
  then "/lib/ld-linux-aarch64.so.1"
  else "/lib64/ld-linux-x86-64.so.2"; # x86_64

dockerImage = pkgs.dockerTools.buildImage {
  name = "ghcr.io/ushitora-anqou/koujouo"; # リリース workflow が公開する名前
  tag = "latest";
  copyToRoot = [
    (pkgs.runCommand "koujouo-root" {} ''
      mkdir -p $out/kojouo
      ln -s ${manager}/bin/manager $out/manager
      ln -s ${manager}/bin/agent $out/kojouo/agent
      # Factorio のバイナリは動的リンクで標準のローダーパスを探す
      mkdir -p "$out$(dirname "${glibcLoader}")"
      ln -s ${pkgs.glibc}${glibcLoader} "$out${glibcLoader}"
    '')
    pkgs.cacert
    pkgs.tzdata
  ];
  config = {
    Entrypoint = [ "/manager" ];       # manager Deployment 用
    User = "65532:65532";
    Env = [ "SSL_CERT_FILE=/etc/ssl/certs/ca-bundle.crt" "TZDIR=/share/zoneinfo" ];
  };
};
```

StatefulSet 側は `command: ["/kojouo/agent"]` を明示するため、image の Entrypoint は manager 用のままでよい。CA 証明書は agent の factorio.com / S3 ダウンロードと、controller の AWS API 呼び出しに必要（両バイナリとも同一 rootfs を使う）。

image のローダーはアーキテクチャで切り替える: agent がダウンロードする Factorio のバイナリは動的リンクで、x86_64 は `/lib64/ld-linux-x86-64.so.2`、aarch64 は `/lib/ld-linux-aarch64.so.1` を探す。`make docker-build` は Nix が作った image をローカルランタイムに load し、`${IMG}` に `docker tag` し直す。

flake 変更後は `nix fmt .` と `nix build .#dockerImage` を必ず回す（AGENTS.md）。

### 2.5 manager と agent の設定（Cobra + Viper）

両バイナリのコマンドラインは root command 1 つだけで、フラグは `--config <path>` のみとする。設定は YAML ファイルで与え、キーは lowerCamelCase、既定値は Viper の `SetDefault` に入れてから構造体へ `Unmarshal` する。フラグ（旧 `--metrics-bind-address`、`--leader-elect`、`--zap-*`、agent の `--listen` など）と環境変数（旧 `KOUJOUO_CONTROLLER_IMAGE`、`KOJOUO_LISTEN` / `KOJOUO_SAVE_DIR` / `KOJOUO_CACHE_DIR`）は廃止した。ファイルが無い・読めない・パースできない場合はエラーで終了する（`SilenceUsage` / `SilenceErrors` を立て、`main` がエラーを stderr に出して exit 1）。

- manager: 既定パス `/etc/koujouo/manager.yaml`。Deployment が ConfigMap として mount する（`controllerImage` はその ConfigMap が持つ）
- agent: 既定パス `/etc/koujouo/agent.yaml`。image に焼き込む（`pkgs.writeText` で作り、`koujouo-root` の `runCommand` が `/etc/koujouo/agent.yaml` に置く）。manager 用の設定ファイルは image に含めない

```yaml
# manager
metricsBindAddress: "0"
healthProbeBindAddress: ":8081"
leaderElection: false
secureMetrics: true
webhookCertPath: ""
webhookCertName: tls.crt
webhookCertKey: tls.key
metricsCertPath: ""
metricsCertName: tls.crt
metricsCertKey: tls.key
controllerImage: ""      # 旧 KOUJOUO_CONTROLLER_IMAGE。空だと StatefulSet を作れない
log:
  development: true
  level: info
```

```yaml
# agent
listenAddress: ":9550"
saveDir: /var/lib/koujouo
cacheDir: /opt/koujouo
log:
  development: true
  level: info
```

ログは `log.development`（コンソール / JSON の選択）と `log.level`（`zapcore.Level.Set` でパースする debug / info / warn / error など）で決め、`zap.Options{Development, Level}` に渡す。全キーと既定値は `examples/manager.yaml` と `examples/agent.yaml` にコメント付きで置く。読み込みは `cmd/manager/config_test.go` と `cmd/agent/config_test.go` で unit テストする（既定値・全キー・欠落とパース失敗）。

設定ファイルの配置は Helm chart（`charts/koujouo`）が担う（manager は chart が生成する ConfigMap を `/etc/koujouo/manager.yaml` に mount、agent は image 内のファイル）。image に manager 用の設定を焼き込まない理由は、`controllerImage` などがリリースごとに変わるため。chart は自身の `image` values を `controllerImage` に書き込むので、manager と agent のイメージは常に一致する。

chart の既定値（`charts/koujouo/values.yaml`）のうち、インストールが成立するために効いているもの:

- `image.tag: latest`。release workflow が push するのは `master`・`latest`・`v*` だけで `Chart.appVersion` のタグは存在しないため、空だと manager が `ImagePullBackOff` になる
- `config.leaderElection: true` と `deploymentStrategy: RollingUpdate` の組み合わせ。rolling update 中は新旧 2 つの manager Pod が同時に動くので、leader election で reconcile する manager を 1 つに保つ
- `imagePullSecrets: []`（manager Pod 用）と `spec.agent.imagePullSecrets`（agent Pod 用）。プライベートレジストリでは両方要る: chart は前者しか設定できない
- Deployment は `POD_NAMESPACE` を downward API で受け取る。これが agent NetworkPolicy の唯一の送信元（§3.3）
- metrics は `config.metricsBindAddress: ":8443"` / `secureMetrics: true`、ServiceMonitor と metrics 用 NetworkPolicy は既定で無効

---

## 3. CRD スキーマ最終案

### 3.1 命名レビュー（プラン案 → 最終案）

Kubernetes API conventions（`sig-architecture/api-conventions.md`）と CloudNativePG（`api/v1/cluster_types.go`、barman-cloud `S3Credentials`）、Velero（`BackupStorageLocationSpec`）、external-secrets（`AWSAuthSecretRef`）の実装に照らして修正する。

| プラン案 | 最終案 | 根拠 |
|---|---|---|
| `gameVersion` | `gameVersion`（そのまま、required） | 問題なし。semver を pattern で強制 |
| `suspend` | `suspend`（そのまま、optional） | external-secrets の `.spec.suspend` と同じ慣習 |
| `leaveTimeout *metav1.Duration` | `leaveTimeoutSeconds *int32`（default 60） | conventions は duration に `fooSeconds` 形式（`fooTimeoutSeconds` を推奨）を定める。CNPG も `smartShutdownTimeout *int32`。`metav1.Duration` は採用しない |
| `saveDataPVCName` | `savePersistentVolumeClaimName`（required） | conventions「略語を使わない」。PVC は K8s API 上 `persistentVolumeClaim` であり、volume の `claimName` に対応する |
| `ephemeralVolumeStorageClassName` | `ephemeralVolume *corev1.EphemeralVolumeSource`（optional） | CNPG の `.spec.ephemeralVolumeSource` と同じく、generic ephemeral volume のネイティブ型を直接受ける。storage class 名だけの独自サブセットより拡張性が高い。`resources.requests.storage` 未指定時は controller がデフォルト（2Gi）を補完する |
| `agent.resources` | `agent.resources corev1.ResourceRequirements`（optional、値型） | CNPG の `Resources corev1.ResourceRequirements` と同一形式 |
| `notification.discordWebhook` | `notification.discordWebhookURL *corev1.SecretKeySelector` | CNPG は URL 系フィールドを `endpointURL` と命名。`corev1.SecretKeySelector`（name + key）が secret 参照の標準形（Velero の `credential`、external-secrets の `xxxSecretRef` と同型。本 CRD では CNPG 流に接尾辞なしで「値の名前 = フィールド名」にする） |
| `backup.endpoint` | `backup.endpointURL`（required） | CNPG `BarmanObjectStoreConfiguration.EndpointURL` と同一命名 |
| `backup.bucket` | `backup.bucket`（required） | Velero `objectStorage.bucket` と同一 |
| `backup.s3Credentials`（2 selector） | `backup.credentials` 構造体 `{accessKeyId, secretAccessKey *corev1.SecretKeySelector}`（両方 required） | CNPG `S3Credentials`（`accessKeyId` / `secretAccessKey`、両方の設定を必須化する CEL/XValidation）と同一。B2 のリージョンは endpoint URL に含まれるため `region` フィールドは省く（SigV4 のリージョンは endpoint URL から解析される。不可欠なら将来追加） |
| `gameConfig.configMapRef` | `gameConfig.config corev1.ConfigMapKeySelector`（required） | キー指定が必要なため `LocalObjectReference` ではなく `ConfigMapKeySelector`。フィールド名は用途ベース |
| `gameConfig.whitelistConfigMapRef` | `gameConfig.serverWhitelist *corev1.ConfigMapKeySelector`（optional） | Factorio 用語は server whitelist。キー指定が必要なため同上 |
| （プランのコメントのみ）`downloadCredentials` | `downloadCredentials {username, token *corev1.SecretKeySelector}`（optional） | experimental/旧版ビルドの `?username=&token=` 用。Secret 参照は SecretKeySelector で統一 |
| status: `phase` + conditions + `currentGameVersion` + `players` | `phase`（string enum）+ `conditions`（metav1.Condition）+ `currentGameVersion` + `onlinePlayers` + `observedGeneration` | conventions では phase は非推奨だが CNPG をはじめ実在 operator で普遍であり、プランが STATUS 列表示を要求するため維持する。機械判定は conditions（`Ready` / `Progressing` / `Suspended`）で行い、phase は人間向けサマリと位置付ける。`players` は CNPG の `readyInstances` 流儀で主語を明確にした |

Secret/ConfigMap の取得責任は controller にあり、取得した値は Cap'n Proto で agent へ渡す（volume mount しない）。したがって RBAC には configmaps/secrets の `get` が必要。

### 3.2 Go struct 最終案

```go
// FactorioServerSpec defines the desired state of FactorioServer.
type FactorioServerSpec struct {
	// gameVersion pins the Factorio release to run, for example "2.1.24".
	// The agent downloads the headless build of this version.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Pattern=`^[0-9]+\.[0-9]+\.[0-9]+$`
	GameVersion string `json:"gameVersion"`

	// savePersistentVolumeClaimName is the name of the existing
	// PersistentVolumeClaim that stores the save data. It must exist
	// before the FactorioServer is created; the controller never
	// creates or deletes it.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	SavePersistentVolumeClaimName string `json:"savePersistentVolumeClaimName"`

	// suspend gracefully stops the game (drain, save) and deletes the
	// StatefulSet. The PersistentVolumeClaim is kept. Defaults to false.
	// +optional
	Suspend bool `json:"suspend,omitempty"`

	// leaveTimeoutSeconds is how long the agent waits for players to
	// leave after warning them before it forces a save and shutdown.
	// Defaults to 60. The maximum leaves room for the save write inside
	// the 900 second termination grace period of the agent Pod.
	// +optional
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=300
	// +kubebuilder:default=60
	LeaveTimeoutSeconds *int32 `json:"leaveTimeoutSeconds,omitempty"`

	// ephemeralVolume is the generic ephemeral volume that holds the
	// downloaded game binary. When set, it overrides the default
	// volumeClaimTemplate. Unset fields (storage class, size) are
	// defaulted by the controller (2Gi).
	// +optional
	EphemeralVolume *corev1.EphemeralVolumeSource `json:"ephemeralVolume,omitempty"`

	// agent configures the agent container of the StatefulSet.
	// +optional
	Agent *AgentSpec `json:"agent,omitempty"`

	// gameConfig points to the ConfigMap keys that hold config.ini and
	// the server whitelist. Their contents are sent to the agent over
	// RPC, not mounted as volumes.
	// +optional
	GameConfig *GameConfigSpec `json:"gameConfig,omitempty"`

	// notification configures Discord notifications for player
	// join/leave events.
	// +optional
	Notification *NotificationSpec `json:"notification,omitempty"`

	// backup configures the periodic upload of the save file to an
	// S3-compatible object storage. Old objects are never deleted.
	// +optional
	Backup *BackupSpec `json:"backup,omitempty"`

	// downloadCredentials references the Secret keys holding the
	// Factorio account username and token, required to download
	// experimental and older builds. Stable builds need no credentials.
	// +optional
	DownloadCredentials *DownloadCredentialsSpec `json:"downloadCredentials,omitempty"`
}

// AgentSpec configures the agent container.
type AgentSpec struct {
	// resources for the agent container (requests/limits).
	// +optional
	Resources *corev1.ResourceRequirements `json:"resources,omitempty"`

	// imagePullSecrets are the Secrets the kubelet uses to pull the
	// image of the agent Pod. They are needed only for an image in a
	// private registry.
	// +optional
	ImagePullSecrets []corev1.LocalObjectReference `json:"imagePullSecrets,omitempty"`
}

// GameConfigSpec selects the game configuration files from ConfigMaps.
type GameConfigSpec struct {
	// config selects the ConfigMap key holding config.ini.
	// +kubebuilder:validation:Required
	Config *corev1.ConfigMapKeySelector `json:"config"`

	// serverWhitelist selects the ConfigMap key holding the JSON server
	// whitelist. When set, the game starts with --use-server-whitelist.
	// +optional
	ServerWhitelist *corev1.ConfigMapKeySelector `json:"serverWhitelist,omitempty"`
}

// NotificationSpec configures Discord notifications.
type NotificationSpec struct {
	// discordWebhookURL selects the Secret key holding the Discord
	// webhook URL. Notifications are disabled when unset.
	// +optional
	DiscordWebhookURL *corev1.SecretKeySelector `json:"discordWebhookURL,omitempty"`
}

// BackupSpec configures save uploads to an S3-compatible bucket.
type BackupSpec struct {
	// endpointURL is the S3 API endpoint, for example
	// "https://s3.us-west-00.backblazeb2.com". Path-style addressing
	// is always used.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Pattern=`^https?://.+$`
	EndpointURL string `json:"endpointURL"`

	// bucket is the destination bucket name.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	Bucket string `json:"bucket"`

	// credentials select the Secret keys holding the S3 access key id
	// and secret access key.
	// +kubebuilder:validation:Required
	Credentials *S3Credentials `json:"credentials"`
}

// S3Credentials references the static S3 credentials.
type S3Credentials struct {
	// accessKeyId selects the Secret key holding the access key id.
	// +kubebuilder:validation:Required
	AccessKeyID *corev1.SecretKeySelector `json:"accessKeyId"`

	// secretAccessKey selects the Secret key holding the secret access key.
	// +kubebuilder:validation:Required
	SecretAccessKey *corev1.SecretKeySelector `json:"secretAccessKey"`
}

// DownloadCredentialsSpec references the Factorio account credentials.
type DownloadCredentialsSpec struct {
	// username selects the Secret key holding the Factorio username.
	// +kubebuilder:validation:Required
	Username *corev1.SecretKeySelector `json:"username"`

	// token selects the Secret key holding the Factorio token.
	// +kubebuilder:validation:Required
	Token *corev1.SecretKeySelector `json:"token"`
}

// FactorioServerStatus defines the observed state of FactorioServer.
type FactorioServerStatus struct {
	// phase is a human-readable summary of the current state.
	// Machine consumers should use conditions instead.
	//
	// +optional
	Phase FactorioServerPhase `json:"phase,omitempty"`

	// currentGameVersion is the game version the agent reports running.
	// +optional
	CurrentGameVersion string `json:"currentGameVersion,omitempty"`

	// onlinePlayers is the number of players currently connected,
	// as reported by the agent.
	// +optional
	OnlinePlayers int32 `json:"onlinePlayers,omitempty"`

	// observedGeneration is the .metadata.generation the status was
	// last reconciled from.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// conditions represent the current state of the FactorioServer.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// FactorioServerPhase describes the operator-level phase.
// +kubebuilder:validation:Enum=Pending;Downloading;Running;Draining;Saving;Suspended;Failed
type FactorioServerPhase string

const (
	PhasePending     FactorioServerPhase = "Pending"
	PhaseDownloading FactorioServerPhase = "Downloading"
	PhaseRunning     FactorioServerPhase = "Running"
	PhaseDraining    FactorioServerPhase = "Draining"
	PhaseSaving      FactorioServerPhase = "Saving"
	PhaseSuspended   FactorioServerPhase = "Suspended"
	PhaseFailed      FactorioServerPhase = "Failed"
)
```

condition の型は `Ready`（Running で True）/ `Progressing`（Pending・Downloading・Draining・Saving で True）/ `Suspended`（suspend で True）の 3 種とする。`reason` は CamelCase で必ず設定する（conventions）。

### 3.3 マーカー・リソース定義

```go
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=fsrv
// +kubebuilder:printcolumn:name="Status",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Version",type=string,JSONPath=`.status.currentGameVersion`
// +kubebuilder:printcolumn:name="Players",type=integer,JSONPath=`.status.onlinePlayers`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
```

RBAC マーカー（controller 側に追加）:

```go
// +kubebuilder:rbac:groups=koujouo.anqou.net,resources=factorioservers,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=koujouo.anqou.net,resources=factorioservers/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=koujouo.anqou.net,resources=factorioservers/finalizers,verbs=update
// +kubebuilder:rbac:groups=apps,resources=statefulsets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=networking.k8s.io,resources=networkpolicies,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch;delete
// +kubebuilder:rbac:groups="",resources=configmaps;secrets,verbs=get;list;watch
// +kubebuilder:rbac:groups=events.k8s.io,resources=events,verbs=create;patch
```

`factorioservers` の `create` と `delete` は付与しない。`FactorioServer` はユーザーのものであり、controller が作るのは子オブジェクト（StatefulSet・NetworkPolicy・Pod）だけである。逆に `pods` の `delete` は必要: `OnDelete` の StatefulSet は pod template の変更を動いている Pod に届けないので、controller 自身が Pod を削除して作り直す（§6）。

StatefulSet の要点（`internal/controller/resources/statefulset.go`）:

- `replicas: 1`、`serviceName: <FactorioServer name>`（Service 自体は作らない）
- `selector`/`template.labels` は `koujouo.anqou.net/factorioserver: <name>`
- `template.spec.containers[0]`: name `agent`、image = controller イメージ（`spec.agent.image` なし。controller が自分のイメージを `Downward API` ではなく manager の設定ファイルの `controllerImage`（Deployment が ConfigMap として mount する `/etc/koujouo/manager.yaml`、§2.5）から受け取る）
- `command: ["/kojouo/agent"]`（PID 1、reaper は agent 自身）
- `ports`: `{name: game, containerPort: 34197, protocol: UDP}` と `{name: rpc, containerPort: 9550, protocol: TCP}`
- `livenessProbe`: 名前 `rpc`（TCP 9550）への `tcpSocket`。initialDelay 10 秒・period 20 秒・timeout 5 秒・failureThreshold 3。agent はゲーム起動前（listener を開いた時点）から RPC を提供し、drain 中も応答し続けるので、接続拒否は agent の停止を意味する。controller は接続を再試行するため initialDelay を置く
- `automountServiceAccountToken: false`（agent は API server と話さない）
- `volumeMounts`: `/var/lib/koujouo`（saves、spec.savePersistentVolumeClaimName）と `/opt/koujouo`（ephemeral、バイナリ）
- `imagePullSecrets`: `spec.agent.imagePullSecrets` をそのまま写す（プライベートレジストリ用）
- `terminationGracePeriodSeconds: 900`（leaveTimeout の上限 300 秒 + セーブ書き込みの待機上限 5 分 + 余裕。pod 削除時に agent が drain と保存を完結させる）
- `securityContext`: nonroot、`seccompProfile: RuntimeDefault`、`allowPrivilegeEscalation: false`、`readOnlyRootFilesystem: true`（書き込みは volume のみ）
- `template.annotations` に `koujouo.anqou.net/template-hash`（下記）

**pod template ハッシュ**: controller が所有する pod template のフィールド（labels と spec）の sha256 を、`agent` コンテナの `image` だけ空にして計算する。`OnDelete` の StatefulSet は template の変更を動いている Pod に届けないため、controller は Pod の annotation を desired なハッシュと比べ、違えば Pod を削除する（StatefulSet が作り直す）。image を除くのは、image の変更がインプレース更新（§6）だけで反映されるべきだから: 含めると controller イメージ更新のたびにゲームが再起動する。裏返しとして、image 以外の所有フィールド（save PVC、resources、ephemeral volume、imagePullSecrets、securityContext、probe）を変えると、ゲームが一度再起動する。

**NetworkPolicy**（`internal/controller/resources/networkpolicy.go`）: agent の RPC は認証なしの平文なので、ingress を「UDP 34197 は任意の送信元」「TCP 9550 は manager の namespace のみ」に絞る。egress は制限しない（ゲームのダウンロード、S3、Discord）。manager の namespace は Deployment の `POD_NAMESPACE`（downward API）から取り、`cmd/manager/main.go` は ServiceAccount の namespace ファイルを fallback にする。`ManagerNamespace` が空（クラスタ外で動く manager）だと NetworkPolicy を作らない: 空の namespace selector はどの送信元も許さず、manager が agent から締め出されるため。この場合 RPC は pod ネットワーク全体から到達可能になり、reconcile が警告を 1 回だけログに出す。NetworkPolicy は `FactorioServer` を owner とするので、削除は GC に任せ、suspend では消さない（resume で作り直す必要がない）。

**Secret / ConfigMap の読み方**: `APIReader`（API server 直、キャッシュなし）で読む。参照される Secret は数が少なく cluster-wide の watch も要らないうえ、manager のメモリキャッシュに Secret を残さずに済む。`readConfigMapKey` / `readSecretKey` はこの reader を使う。

**並行度と status の期限**: `MaxConcurrentReconciles` は 8。configure は drain とダウンロードで数分かかり、reconcile はその完了を待つので、worker が 1 つだと他の `FactorioServer` の reconcile がその間ずっと止まる（workqueue は同じ key を並行実行しないため、1 つのサーバーの reconcile は直列のまま）。`status` 呼び出しは 30 秒（`defaultAgentStatusTimeout`）で打ち切る: 応答しない agent が 1 回のポーリング分だけを費やし、待機全体を占領しない。

---

## 4. capnp スキーマ最終案（`rpc/koujouo.capnp`）

```capnp
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
  agentVersion @3 :Text; # build version of the running agent binary
}

interface Agent {
  # configure applies the desired state. It is idempotent. On a game
  # version change it drains the players, stops the game, downloads the
  # new binary and starts it before returning.
  configure @0 (cfg :Config) -> ();

  # status reports the current agent/game state. The controller polls it.
  status @1 () -> (st :AgentStatus);

  # stop gracefully stops the game (drain + save). Returns after the
  # save file has been written and the game process has exited.
  stop @2 () -> ();

  # upgrade replaces the agent binary: the agent returns a ByteStream
  # capability, the controller pushes the new binary through it and ends
  # the stream with done(sha256). The agent verifies the sha256, then
  # execve(2)s the new binary. The Factorio child process keeps running;
  # its pid travels in the environment.
  upgrade @3 () -> (bin :ByteStream);
}

interface ByteStream {
  # Streaming method: resolve the previous write before sending the
  # next chunk, so the RPC layer applies backpressure.
  write @0 (chunk :Data) -> Stream.StreamResult;
  done  @1 (sha256 :Data) -> ();  # 32 bytes; ends the stream
}
```

プロトコル上の注意:

- `configure` は常に「フル状態」を送る（差分計算を agent 側でしない）。冪等性のため。
- 認証なし plain TCP は pod ネットワーク内に限る。agent は `0.0.0.0:9550` で待つ。controller は agent Pod ごとに NetworkPolicy を作り、TCP 9550 を manager の namespace からのみ許可する（UDP 34197 は任意の送信元）。`ManagerNamespace` が空のときだけ NetworkPolicy を作らず、RPC は pod ネットワーク全体から到達可能になる（§3.3）。
- `stop` は RPC の return を待つことでセーブ完了を保証する。controller は `suspend` 時に `stop` を呼んでから StatefulSet を削除する。
- スキーマ変更時は `make generate-capnp` で `rpc/koujouo.capnp.go` を再生成し、コミットする（生成物はリポジトリに含める。CI で `capnp` を実行しない）。

---

## 5. ファイルレイアウト

```
rpc/
  koujouo.capnp            新規  上記スキーマ
  koujouo.capnp.go         生成  capnpc-go 出力（コミットする）
  generate.go              新規  //go:generate ディレクティブのみ

api/v1/
  groupversion_info.go     修正  groupName を koujouo.anqou.net へ
  factorioserver_types.go  修正  §3.2 の spec/status に置換（Foo 削除）
  zz_generated.deepcopy.go 自動  make generate

cmd/
  manager/main.go          移動  cmd/main.go から（Cobra + Viper、--config のみ）
  agent/main.go            新規  agent エントリ: Cobra + Viper、--config のみ、
                                  /var/lib/koujouo の状態復元、rpc.Serve

examples/
  manager.yaml             新規  全キーと既定値（コメント付き）
  agent.yaml               新規  全キーと既定値（コメント付き）

internal/controller/
  factorioserver_controller.go 修正 Reconcile 本体、SetupWithManager(.Owns(StatefulSet)+Watches(Pod))、
                                   pod template ハッシュの比較と Pod の再作成
  agentclient.go           新規  dial(ctx, pod) (rpc.Agent, error)、AgentDialer interface
  status.go                新規  syncStatus(server, agentStatus)（phase/conditions/versions）
  resources/
    statefulset.go         新規  buildStatefulSet(server, controllerImage)（pod template ハッシュ、
                                   liveness probe、ephemeral volume）
    networkpolicy.go       新規  buildNetworkPolicy(server, managerNamespace)（UDP 34197 は任意、
                                   TCP 9550 は manager namespace のみ）

internal/agent/
  agent.go                 新規  Agent_Server 実装（configure/status/stop/upgrade の
                                  ディスパッチ、状態管理、子プロセスの reaper）
  config.go                新規  Config/受信ファイルの一時ファイル化、save volume への
                                  キャッシュ（agent-state.json）と再起動時の復元
  download.go              新規  ensureBinary(version, creds): get-download → tar.xz 展開
  factorio.go              新規  startFactorio(): 子プロセス起動
                                  (-c config.ini --start-server saves/map.zip
                                   --rcon-bind 127.0.0.1:55555 --rcon-password <random>
                                   [--use-server-whitelist --server-whitelist ...])
  stop.go                  新規  stopGraceful(): rcon 警告 → 全員退出 or timeout →
                                  SIGINT → セーブ完了待ち（SIGTERM ハンドラもここ）
  upgrade.go               新規  ByteStream 受信、sha256 検証、reexec(execve(2)、
                                  KOUJOUO_FACTORIO_PID 環境変数で pid 引き継ぎ)
  logs.go                  新規  [JOIN]/[LEAVE] パース、agent ログへのミラー
  notify.go                新規  Discord webhook POST（リトライ付き）
  backup.go                新規  backupLoop（1 時間毎 + graceful stop 後）、S3 PutObject
  rcon.go                  新規  gorcon の薄ラッパ（PlayersOnline/Chat）

charts/koujouo/
  Chart.yaml               新規  apiVersion v2 / type application / version 0.1.0 / appVersion "0.0.1"
  values.yaml              新規  image・manager の設定・securityContext・ServiceMonitor・NetworkPolicy
  templates/               新規  Deployment・ConfigMap・ServiceAccount・RBAC・metrics Service など
  templates/koujouo.anqou.net_factorioservers.yaml  自動  make manifests（CRD）
  templates/manager-role.yaml                       自動  make manifests（manager の ClusterRole）

examples/
  pvc.yaml                 移動  config/samples/ から（保存用 PVC）
  configmap.yaml           移動  config/samples/ から（config.ini と whitelist）
  secret.yaml              移動  config/samples/ から（プレースホルダーの Secret）
  factorioserver.yaml      移動  config/samples/ から（FactorioServer のサンプル CR）
  README.md                新規  適用順（PVC → ConfigMap/Secret → FactorioServer）

test/e2e/
  e2e_suite_test.go        修正  Helm chart でのデプロイ（`helm upgrade --install` / `helm uninstall`）と CRD の後始末
  factorio_e2e_test.go     新規  §7 の e2e シナリオ

flake.nix                   修正  §2（go2nix の manager、devShell、go2nix プラグイン、image、agent の設定ファイル）
Makefile                    修正  §2.2（generate-capnp、build/run のパス）、§7.4（all / check-uncomitted）、release / release-image / release-chart（§7.4 のリリース workflow と同じ archive）
.github/workflows/ci.yml    新規  §7.4（make all + check-uncomitted の 1 workflow。lint.yml / test.yml を統合）
.github/workflows/e2e.yml   新規  §7.4（make test-e2e だけを回す workflow: PR、master push、日次、手動）
docs/design.md              本書
```

controller の `agentclient.go` は interface 化（`type AgentDialer interface { Dial(ctx, *corev1.Pod) (rpcapi.Agent, error) }`）し、本物の TCP dial 実装とテスト用 fake（in-process の capnp サーバーを `net.Pipe` で繋ぐ）を差し替え可能にする。envtest では fake を使う。

---

## 6. 動作フロー（要約）

**通常の Reconcile**

1. `ensureAgentObjects(server)` — `ensureStatefulSet` と `ensureNetworkPolicy`。suspend なら agent.stop → STS 削除 → Suspended condition。PVC と NetworkPolicy は触らない（NetworkPolicy は FactorioServer を owner とするので GC が消す）。
2. pod (agent) の IP を取得し `dial`。失敗時は `RequeueAfter`（短间隔）。
3. `Agent.status()` を取得。`gameVersion != spec.gameVersion` なら `Agent.configure(fullConfig)` を呼び、返ってくるのを待つ（agent が drain → download → restart を実施）。それ以外は差分（files 等）があれば `configure`。
4. `syncStatus` で phase / conditions / currentGameVersion / onlinePlayers / observedGeneration を更新。
5. 定期 requeue（例: 30 秒）で onlinePlayers を更新。

**agent の自己復帰**: pod 再起動時、agent は save volume の `agent-state.json`（最後に受け取った Config を保存）から復元し、controller を待たずにゲームを起動する。controller の dial が成功した時点で `configure` により最新化される。

**agent upgrade**: controller は `status` の `agentVersion` と自身のビルド（`internal/version.Agent`。flake と Makefile が `-X` で git short rev を注入）を比較し、不一致なら `upgrade()` を呼ぶ。戻り値の `ByteStream` に対し、manager はイメージ内 `/kojouo/agent` を 512KiB ごとに `write` し（毎回 `WaitStreaming()` で待つ）、最後に `done(sha256)` を呼ぶ。agent は `/opt/koujouo/agent.next` に書き出し、sha256 を検証し、`syscall.Exec` で置換する。Factorio は止まらない。

**configure / stop の待機と進捗**: controller は `configure()` / `stop()` の完了を待つ間、2 秒ごとに `status()` をポーリングし、phase の変化（`downloading` / `draining` / `saving`）をそのつど FactorioServer の status に反映する。agent はこれらのハンドラの先頭で `call.Go()` を呼び、処理中も `status` に応答する。待機と status のパッチは reconcile のタイムアウト（controller-runtime 既定 10 分）に縛られない（`context.WithoutCancel`）。drain は leaveTimeout のぶんだけ待つためで、上限の `leaveTimeoutSeconds: 300` でも切断されない。

**StatefulSet の更新戦略**: in-place upgrade を活かすため `updateStrategy: OnDelete` とする。既定の `RollingUpdate` だと controller イメージ更新のたびに pod が roll され Factorio が再起動する。`OnDelete` は pod template の変更も動いている Pod に届けないので、controller が pod template のハッシュ（§3.3）を比べて Pod を削除し、StatefulSet に作り直させる。image の変更だけはハッシュに含めないため、インプレース更新の経路だけで反映する。

**既定の config.ini**: ゲームは常に `-c <saveDir>/config.ini` で起動する。`spec.gameConfig` は任意なので、config.ini を配信しない設定では agent が `[path]\nwrite-data=<saveDir>` だけの config.ini を書く。これが無いとゲームは write-data を image 内の既定パスに置き、autosave が save volume の外に出て Pod の入れ替えで失われる。配信された config.ini の `write-data` も、既存の `rewriteWriteData` が saveDir に書き換える。

**agent の終了と復帰**: `Shutdown`（SIGTERM）は `terminating` フラグをロック取得前に立て、期限付きで lifecycle ロックを待つ。ダウンロード中の configure がロックを保持していても、configure はゲームを起動する直前にフラグを見て中断し、`Shutdown` は期限で戻る（pod の grace period を超えない）。`terminating` は configure がロックを取ったあとにも効く必要があるので atomic にしてある。逆に resume（controller を待たずキャッシュ済み設定で起動する経路）は、一時的な失敗で `Failed` のまま固まらないよう 3 回まで指数バックオフで再試行する: controller は resume が置き換えた configure 呼び出しを繰り返さない。

**ゲーム異常終了による agent の終了**: 要求されていないゲームの終了（クラッシュ、OOM kill など）は、ゲームだけを `failed` にして agent を動かしたままにしない。`watchGame` は自分がまだ持っているゲームの終了だけを `Agent.Fatal()` のチャネルに `errUnexpectedGameExit` として流し、`cmd/agent` の `waitForSignal` はシグナル・RPC サーバーのエラーと並べてこのチャネルを待つ。fatal を受けると listener を閉じ、`shutdown` を呼ぶ（ゲームは既に終わっているので stop は何もしない）うえで fatal のエラーを返すため、agent は非ゼロで終了し、`restartPolicy: Always` の kubelet がコンテナを再起動する。新しい agent は `Resume` → `resumeFromState` で save volume の `agent-state.json` から復帰し、controller の `configure` を待たずにゲームを起動する。繰り返し落ちるゲームは CrashLoopBackOff として観測できる。要求された stop（SIGTERM、`stop`、バージョン変更）は fatal 経路に入らない: `watchGame` は自分が持っているゲームだけを報告し、`stopLocked` は終了が見える前にハンドルをクリアする。resume 自体の失敗（ダウンロード・起動の失敗）はゲームを起動しなかっただけなので fatal ではなく、従来どおり 3 回の再試行で諦める。

**ゲーム出力の読み取り**: `copyGameOutput` は行単位で agent ログに写し、`[JOIN]` / `[LEAVE]` を Discord へ流す。読み取りがゲームの書き込みを止めないことが最優先なので、(a) `bufio.Scanner` のバッファを 64 KiB から開始し 1 MiB の行まで受け付け、(b) scanner が諦めた残りを `io.Copy(io.Discard, reader)` で読み捨て、(c) 通知は容量 64 のキューと送信ゴルーチンに分離し、キューが溢れたら通知を捨てる（webhook が詰まってもゲームを止めない）。

**save volume の権限**: pod に `fsGroup: 65532` を付ける。root 所有の volume を払い出す CSI でも、nonroot の agent が書き込れるようにするため。

**Secret の読み方**: controller は参照された ConfigMap/Secret を `APIReader`（キャッシュなし）で読む。数が少なく cluster-wide watch も不要で、manager のキャッシュに Secret を残さずに済む。読み取った値は `Config` 構造体で agent へ RPC で渡し、Pod には mount しない（§3.1）。

**docker image と glibc**: 公式 Factorio headless バイナリは動的リンク（`/lib64/ld-linux-x86-64.so.2` と glibc が必要）。docker image には glibc を同梱し、`/lib64/ld-linux-x86-64.so.2` を `${pkgs.glibc}/lib/ld-linux-x86-64.so.2` へのファイル symlink として置く。

---

## 7. テスト計画

### 7.1 unit（`go test`、envtest 不使用）

| 対象 | ファイル | 内容 |
|---|---|---|
| ログパース | `internal/agent/logs.go` | `[JOIN] anqou joined the game` / `[LEAVE] ... left the game` の正規表現（実ログの形式に対するテーブルテスト）。加えて出力読み取りが止まらないこと: 64 KiB を超える長い行（`TestCopyGameOutputHandlesLongLines`）、応答しない webhook（`TestCopyGameOutputDoesNotBlockOnASlowWebhook`）、1 MiB 超の行のあとの読み捨て（`TestCopyGameOutputDiscardsTheRestOfAnOverlongStream`） |
| graceful stop 状態機械 | `internal/agent/stop.go` | プレイヤー 0 人なら即 SIGINT、1 人以上なら rcon 呼び出し→退出検知/タイムアウト。rcon とプロセスは偽物（interface）。`leaveTimeoutSeconds: 0` は待たない |
| tar.xz 展開 | `internal/agent/download.go` | 小さな .tar.xz を fixture に展開検証、パストラバーサ (`../`) 拒否 |
| Discord 投稿 | `internal/agent/notify.go` | httptest サーバで body/ヘッダ/リトライを検証 |
| S3 アップロード | `internal/agent/backup.go` | S3 mock エンドポイントは自作せず httptest で SigV4 リクエストを受けて key/body を検証（外部ミニオ不要）。失敗経路（5xx を返す endpoint、セーブ無しの saves ディレクトリ）で `backupOnce` が戻り、`backupLoop` が ctx で止まること |
| capnp プロトコル | `rpc/` + `internal/agent` | `net.Pipe` 上で本物の rpc.Serve/NewConn を使い、configure/status/stop/upgrade（小バイナリ）の往復を検証。`Shutdown` が期限で戻ること（`TestShutdownReturnsWhenTheConfigureHoldsTheLock`）、resume の一時失敗の再試行（`TestResumeRetriesATransientFailure`）。ゲームの異常終了が `Fatal()` に届くこと（`TestAgentReportsACrashedGame`）と、要求された stop が fatal 経路に入らないこと（`TestAgentKeepsAGracefulStopOutOfTheFatalChannel`） |
| 既定 config.ini | `internal/agent/config.go` | 配信ファイルが空のときに `<saveDir>/config.ini` の `write-data` が saveDir を指すこと、配信された config.ini を上書きしないこと |
| state 復元 | `internal/agent/config.go` | キャッシュ JSON の往復 |
| CRD 変換/デフォルト | `api/v1` | LeaveTimeoutSeconds のデフォルト等 |

### 7.2 integration（envtest、`make test`）

`make test` は最初に `make secrets` で gitleaks に git 履歴をスキャンさせる（作業ツリーではない。未追跡のローカルファイルはリポジトリの一部ではないため）。`--redact` を付けるのは、検出した秘密情報を CI のログに出さないためである。

controller のロジックを実際の API server で検証。agent は動かないため `AgentDialer` に in-process fake を注入する。

- FactorioServer 作成 → StatefulSet（replica 1、command、volumes、labels、probe、`automountServiceAccountToken: false`）が正しく作られる
- pod template のハッシュ annotation が desired と一致し、2 回目の reconcile が StatefulSet を書き換えない（resourceVersion が変わらない）
- pod template の変更（`savePersistentVolumeClaimName`、`spec.agent.resources`）で Pod が削除され、新しい template の StatefulSet が残る。image だけの変更では Pod が削除されない。controller が所有しない annotation は保たれる
- NetworkPolicy: UDP 34197 が任意の送信元、TCP 9550 が manager namespace（`kubernetes.io/metadata.name`）のみ、egress は無制限、suspend しても残る。`ManagerNamespace` が空だと作られない
- suspend=true → fake agent.stop が呼ばれ、StatefulSet が削除され、PVC が残る。dial が失敗する場合は StatefulSet を消さず reconcile がエラーを返す。Pod が Ready でない場合は stop を呼ばずに削除する
- `callAgentStop` は `context.WithoutCancel` で呼ぶため reconcile の期限では戻らない（`agentstop_internal_test.go`）。期限で戻すと、ゲームが動いたまま StatefulSet を消すことになる
- gameVersion 変更 → configure が新しい gameVersion で呼ばれる
- status の反映（phase、conditions、observedGeneration）。suspend のあとに resume して agent がまだ起きていない場合、`Suspended` 条件が残らず `Pending` になる（`status_internal_test.go` と controller の "should report Pending while the resumed agent Pod is missing"）
- ゲーム異常終了で pod が再起動している間は dial が失敗するので `Pending`（`AgentUnavailable`）になり、再起動した agent がキャッシュから復帰して Running を報告すると `configure` 無しで Running に戻る（"should continue with the agent that resumed the cached configuration"）
- ConfigMap/Secret 参照の解決とエラー（NotFound→condition）
- RBAC: `make manifests` で生成された role が envtest で機能することは検証しない（e2e でカバー）

### 7.3 e2e（kind、`make test-e2e`）

既存の `test/e2e` 枠組み（Kind + `make docker-build IMG=example.com/koujouo:v0.0.1`）を使うが、デプロイは `make install` / `make deploy`（もう存在しない）ではなく Helm chart で行う。`BeforeSuite` が次の順で 1 回だけ実行し、2 つのコンテナが同じ operator を使う。

1. `make docker-build IMG=<managerImage>` で image をビルドする
2. `make docker-build-e2e-old IMG=<oldManagerImage>` で「古いイメージ」をビルドする。flake の `dockerImageE2EOld` は同じソースから version だけ `e2e-old` にした manager と agent を持つ（`mkManager` / `mkDockerImage` に version を引数で渡す。名前と tag は `dockerImage` と同じで、suite が load 後に re-tag する）
3. `utils.LoadImageToKindClusterWithName` で 2 つの image を Kind に load する
4. Namespace `koujouo-system` を作り（既にあれば再利用する）、`restricted` の Pod Security Standards を enforce する（chart は Namespace を作らない）
5. 前回の失敗が残した CRD を消す（`kubectl delete crd factorioservers.koujouo.anqou.net --ignore-not-found`）。CRD は `helm.sh/resource-policy: keep` を持つため `helm uninstall` では消えず、release が所有しない CRD が残っていると Helm の install が失敗する
6. `helm upgrade --install koujouo charts/koujouo --namespace koujouo-system --set image.repository=<repo> --set image.tag=<tag>` でデプロイする。`managerImage` は `<repo>:<tag>` なので、registry のポートを repository 側に残すため最後の `/` より後ろの `:` で repository と tag に分ける。**tag は旧イメージ（`oldManagerImage`）のもの**を使う: 初期の manager と agent は `e2e-old` で動き、シナリオ 3 が release を `managerImage` に upgrade する
7. `kubectl wait deployment/koujouo-controller-manager -n koujouo-system --for=condition=Available --timeout=5m` で待つ（Deployment 名・ServiceAccount 名・metrics Service 名・`koujouo-metrics-reader` ClusterRole 名は chart が `fullnameOverride: koujouo` で作る名前と一致する）

`AfterSuite` は `helm uninstall koujouo --namespace koujouo-system`（失敗しても続行）→ CRD の削除（keep annotation のため uninstall では消えない）→ manager Namespace の削除 → cert-manager の teardown の順で行う。CertManager は不要（webhook が無い）なので、`make test-e2e` は `CERT_MANAGER_INSTALL_SKIP=true` を渡す。suite は image 2 つのビルドと Factorio の 2 回のダウンロードを含むため、`go test` の既定 10 分では足りず、`-timeout`（`E2E_TIMEOUT`、既定 60m。agent upgrade の分だけ増やしてある。e2e.yml の job は `timeout-minutes: 90`）を指定する。

| コンテナ | ファイル | 内容 |
|---|---|---|
| `Manager` | `e2e_test.go` | スキャフォールドの metrics テストをそのまま残す（旧 manager でも通る） |
| `FactorioServer`（`Ordered`） | `factorio_e2e_test.go` | 下の 6 シナリオをこの順で実行する |

`FactorioServer` は専用 Namespace `koujouo-e2e-factorio`（Pod Security `restricted` を enforce）に PVC・ConfigMap（config.ini・server-whitelist.json）・FactorioServer を作る。バックアップと `downloadCredentials` は設定しない（どちらも optional で、未設定なら agent はアップロードも認証付きダウンロードもしない）。

| # | シナリオ | 検証内容 |
|---|---|---|
| 1 | downloads the game and hosts the world | StatefulSet（replicas 1・`OnDelete`・`fsGroup: 65532`・`command: /kojouo/agent`・image は旧イメージ `oldManagerImage`・volume/mount）ができること。agent Pod が Running になり、`status.phase == Running`、`currentGameVersion == 2.1.24`、`observedGeneration == generation`、Ready 条件が True になること（タイムアウト 15 分。ダウンロードと展開を含む）。agent ログに `Extracted the game archive`、`No save found, creating one`（初回のみ）、`Started Factorio`、`Hosting game at IP ADDR:({0.0.0.0:34197})`、`Starting RCON interface at IP ADDR:({127.0.0.1:55555})`（ゲームポート UDP 34197 と RCON の bind を示す Factorio の起動行）があること。`Factorio exited unexpectedly` と Discord 通知失敗（`Could not notify Discord`）が無いこと |
| 2 | stores the save file and the agent state on the save volume | PVC を read-only で mount した一時 Pod（`test/utils.RunVolumeInspector`）で `agent-state.json`、`config.ini`、`server-whitelist.json`、`saves/map.zip` の存在を確認し、map.zip の size・mtime・sha256 を記録する |
| 3 | upgrades the agent without stopping the game | 旧イメージ（version `e2e-old`）の manager と agent が動いている状態で、`helm upgrade koujouo charts/koujouo -n koujouo-system --reuse-values --set image.tag=<managerImage の tag>` を実行し、`kubectl rollout status deployment/koujouo-controller-manager` を待つ。`OnDelete` の StatefulSet は Pod を残すので、新しい manager が旧 agent に `/kojouo/agent` をストリームし、agent が execve(2) する。検証内容: (a) upgrade 前の Pod UID と container の `restartCount` を記録し、後で同じであること（`DeletionTimestamp` が nil）、(b) `agentLogs.Done` が閉じない（コンテナが終了しない）こと、(c) upgrade 前のオフセット以降の agent ログに `Execing the new agent binary` と `Resumed the game after an agent upgrade` があり、`Started Factorio` と `Extracted the game archive` が無いこと、(d) ログ全体の `Started Factorio` が 1 回だけであること（ゲームが再起動していない）、(e) `phase == Running` かつ `currentGameVersion == 2.1.24` のままであること、(f) StatefulSet の pod template の image が `managerImage` に更新されること（将来の Pod 用に manager が image を書き換えた）。(c) は 10 分待つ |
| 4 | moves the game to another version | `spec.gameVersion` を `2.1.25` に変更し、Running かつ `currentGameVersion == 2.1.25` になること。変更後の agent ログに `No players are online`（drain が RCON `/players online count` に成功した証拠）、`Saving map as` と `Goodbye`（graceful stop）、`Extracted the game archive`（新バージョンの取得）、`Started Factorio` と `Hosting game` があること。`No save found, creating one` が無いこと |
| 5 | suspends the game and keeps the save volume | `spec.suspend: true` で agent が終了し、StatefulSet と Pod が削除され、`phase == Suspended`・Suspended 条件 True・`onlinePlayers == 0` になること。agent ログに `No players are online`、`Saving map as`、`Goodbye`、`Received signal, stopping the game gracefully`、`Stopped agent` があること。PVC は Bound のまま残ること。map.zip の mtime がシナリオ 2 より新しいこと（graceful stop がセーブを書き直した） |
| 6 | resumes the game from the existing save | `spec.suspend: false` で Pod が再作成され（generic ephemeral volume は Pod と共に消えるため再ダウンロードする）、Running かつ `2.1.25` に戻ること。新 Pod の agent ログに `No save found, creating one` が無いこと。map.zip の size・mtime・sha256 がシナリオ 5 の記録と完全に一致すること |

実装上の要点:

- agent ログは `test/utils.FollowPodLogs`（`kubectl logs -f` をメモリに取り続ける）で追いかける。Pod は suspend で削除されるため、削除後の assert はフォロワのバッファに対して行い、バージョン変更の前後比較はバッファのオフセットで行う
- `agent-state.json` には webhook URL と RCON パスワードが入るため、一時 Pod はファイル名の一覧と map.zip の fingerprint だけを出力し、ファイル内容は出力しない
- 失敗時は `AfterEach` が FactorioServer の YAML、StatefulSet と Pod の describe と logs（previous を含む）、PVC、PV、events、controller-manager のログ、追跡した agent ログ、status の phase 遷移を出力する
- RCON の証拠: `status.onlinePlayers` は 0 のとき省略される（`omitempty`）ため、単独では RCON 成功を証明しない。drain 時（シナリオ 4・5）の `No players are online` で `/players online count` が `(0)` を返したことを確認する（RCON 失敗時は `Could not query the online players` になる）
- upgrade（シナリオ 3）はログのオフセットをローカル変数で持つ。`logOffset` はシナリオ 4 のバージョン変更が使うため上書きしない。upgrade 前後でログが途切れないのは、agent がコンテナの中で execve(2) するだけで、コンテナの stdout を `kubectl logs -f` が追い続けるためである
- 再実行性: 2 つの Namespace（`koujouo-system` と `koujouo-e2e-factorio`）は存在を確認してから作る。`Manager` コンテナの `koujouo-metrics-binding` ClusterRoleBinding は cluster-scope で Namespace の削除に巻き込まれないため、`kubectl delete clusterrolebinding ... --ignore-not-found` してから create する（SA に metrics-reader を付ける意図は変えない）

環境変数の扱い:

- `DISCORD_WEBHOOK_URL`: 設定時のみ Secret を作り、`spec.notification.discordWebhookURL` から参照する。未設定なら通知なしで動く（skip しない）。URL はコード・workflow・コミット対象のファイルには書かない
- e2e の Factorio ダウンロードは stable（認証不要）を使うため `downloadCredentials` は設定しない

Discord 通知の検証範囲: join/leave はプレイヤーの接続が必要なため e2e では発生しない。確認できるのは「webhook URL を Secret 経由で agent に配信した状態で、通知失敗（404 等）が agent ログに出ないこと」までである。join/leave の通知経路（`[JOIN]`/`[LEAVE]` の解析から webhook POST まで）は unit テスト（`internal/agent/logs_test.go`、`notify_test.go`）でしか検証できておらず、実運用での確認が残る（§8.6）。

### 7.4 CI

`.github/workflows/ci.yml` の 1 workflow・1 job（`ubuntu-latest`、`timeout-minutes: 30`）で、コミット前にローカルで通すのと同じゲートを回す。step は checkout（SHA 固定）、Nix install（`cachix/install-nix-action`、`install_url` / `install_sha256` で upstream 2.34.9 に固定）、dev shell への enter（`nicknovitski/nix-develop`）、`go mod tidy`、`make all`、`make check-uncomitted` の順で、master への push と pull request のたびに実行する（PR ブランチへの push は pull_request と重複するため対象外）。以前の `lint.yml` / `test.yml` は `ci.yml` に、`test-e2e.yml` は `e2e.yml` に引き継いだ。go2nix のプラグインは nixpkgs の Nix 2.34.9 に対してビルドされており、別ビルドの Nix では `undefined symbol: nix::eBlackHole` で読み込めないため、flake.lock の `nixVersions.nix_2_34` を更新するときはこの固定も合わせる。

- `make all`: `fmt generate manifests lint-config lint test`。生成物（DeepCopy・capnp・chart の CRD と ClusterRole）の再生成、golangci-lint の設定検証（`lint-config`）と lint（`golangci-lint` + `helm lint`）、unit + envtest（`make test`。最初に gitleaks が git 履歴をスキャンするため、checkout は `fetch-depth: 0` で全履歴を取得する）を順に実行する。`build` は含めない: `test` が manager と agent のコンパイルを兼ねるため。e2e（`make test-e2e`）は含めない: Docker、Kind クラスタ、image のビルドと Factorio のダウンロードが要り最大 60 分かかるので、`e2e.yml` が回す。`timeout-minutes: 30` は lint と unit + envtest に envtest バイナリのダウンロードを足した分数に余裕を見た値。
- `make check-uncomitted`: `fmt generate manifests` を実行した後に `git status --porcelain` を見て、追跡ファイルの変更と未追跡ファイルの両方で失敗する。CI では `go mod tidy` が `make all` の前の step なので、tidy が差分を生む `go.mod` / `go.sum` もここで検出される。生成物のコミット漏れを CI で検出するのはこれが初めてで、以前の「workflow は生成物を比較しないので自分で make してコミットすること」という運用を置き換える。ユーザーが別の未コミット変更を持っている場合も失敗するのは意図どおり（クリーンなツリーで実行する）。綴りは `check-uncommitted` ではなく `check-uncomitted`（ユーザー指定のターゲット名）。

`e2e.yml` は `make test-e2e` だけを回す workflow で、pull request、master への push、日次（18:00 UTC = 03:00 JST）、手動 dispatch で起動する。step は `ci.yml` と同じ checkout（`fetch-depth: 0`。flake が git の revision を image に焼き、追跡ファイルだけをビルドするため）/ Nix install / dev shell / `go mod tidy` のあと `make test-e2e` を実行する。`DISCORD_WEBHOOK_URL` は repository secret からこの workflow だけが渡す（secret が無ければ空になり、suite は Secret を作らずに通知なしで動く）。job は `timeout-minutes: 90`（`E2E_TIMEOUT` の 60m と image のビルド・クラスタの後始末の分）。失敗時は suite のログを artifact に残し（`tee e2e.log` と `actions/upload-artifact`）、クラスタの状態（`kubectl get all -A`、events、manager のログ、Pod）を出力し、成否にかかわらず Kind クラスタ `koujouo-test-e2e` を削除する（成功時は `test-e2e` 自身が消すので `|| true`）。workflow は ref ごとに `concurrency` で 1 本に絞り、新しい commit が前の run を cancel する。

runner は `ubuntu-latest` の Docker を使うため、`make docker-build`（`nix build .#dockerImage` → `docker load` → tag）と `kind load docker-image`、kind 内での `curlimages/curl` と `busybox` の pull がそのまま動く（これらは `e2e.yml` の仕事）。e2e のテストファイルは `//go:build e2e` のため `make lint` の対象外（`test/utils` は対象内）。flake は go2nix を lockfile 無しで使うため（§2.3）`vendorHash` の更新は無く、`go.mod` / `go.sum` を更新する PR もそのままビルドできる。CI は dev shell の中で `make` を回すので、go2nix の Nix プラグインは shellHook が設定する `NIX_CONFIG` 経由で読み込まれる。

**リリース workflow（`.github/workflows/release.yml`）**: `master` への push と `v*` tag の push で image を GHCR（`ghcr.io/ushitora-anqou/koujouo`、§2.4 の名前）へ publish する。Dockerfile は使わず、flake の `.#dockerImage` を runner 上でビルドする。job は 4 つ（`chart` は `v*` tag のみ）:

1. `meta`: `nix eval --json .#dockerImage --apply 'p: {repo = p.imageName; tag = p.imageTag;}'` でリポジトリ名を取り、`docker/metadata-action` で最終タグを決める。`master` push は `master` と `latest`、`v*` tag は `vX.Y.Z` のみ（`latest` は default branch に限るので、古い tag で `latest` が巻き戻らない）
2. `build`: `linux/amd64`（`ubuntu-latest`）と `linux/arm64`（`ubuntu-26.04-arm`）の matrix。`nix build .#dockerImage` は runner の native system を評価するので cross も emulation も無い。各 arch は `<version>-linux-<arch>` を push し、digest を artifact で `merge` に渡す
3. `merge`: `docker buildx imagetools create` で per-arch digest から最終タグを原子的に作る（buildx は v0.31.0 に固定。v0.31.1 以降 `imagetools create` が GHCR で壊れているため: docker/buildx#3708）。最後に `imagetools inspect` で検証する
4. `chart`: `v*` tag のみ（`if: startsWith(github.ref, 'refs/tags/v')`、`needs: [meta]`）。`meta.outputs.version` の `v` を落として chart のバージョン（`v0.1.0` → `0.1.0`。接頭辞が無ければ失敗させる）を求め、`helm lint` のあと `make release-chart RELEASE_VERSION=<version>` を実行して `dist/koujouo-<version>.tgz` を作る。GitHub Release が無ければ `gh release create`、既にあれば `gh release upload --clobber` で archive を添付する。job は `permissions: contents: write` を持ち、失敗を握り潰す step は無い（chart の無い release は release として不完全なので run を赤にする）。`build`/`merge` の完了は待たないので、失敗すると image だけが publish された状態になりうる

image の 3 job のうち `meta` と `build` は `make` を通さず `nix build` を直接呼ぶが、go2nix の Nix プラグイン（§2.3）が評価に要るので dev shell に入る（`merge` は digest を Docker で束ねるだけでどちらも要らない）。`chart` も dev shell に入り、`make release-chart` を通す（ローカルの `make release` と同じ archive を作るため）。`latest` が付くのは `master` への push だけなので、`v*` tag のビルドが済んだ image をそのまま配るには `vX.Y.Z` を指す。chart は GitHub Release 以外に publish しない（GHCR の chart repository は持たない）。

---

## 8. 懸念・未確定事項

実装時に解決したものは「→」のあとに結果を記す。

1. **capnp v3 が alpha タグ**: `v3.1.0-alpha.2` に依存する。破壊的変更の可能性は残る。生成コードと RPC API への依存は `rpc/`・`internal/controller/agentclient.go`・`internal/agent/agent.go` に局所化してある。
2. **`capnpc-go` の Nix 化**: → 取りやめ。`capnpc-go` は go.mod の `tool` ディレクティブで管理し、`go tool capnpc-go` として実行する（flake はビルドしない）。`make generate-capnp` で再生成できる。
3. **controller イメージ名の伝達**: → manager の設定ファイルの `controllerImage`（Deployment が ConfigMap として mount する `/etc/koujouo/manager.yaml`）で与える（§2.5）。環境変数 `KOUJOUO_CONTROLLER_IMAGE` は廃止した。
4. **`players` の集計**: → agent が 10 秒ごとに rcon `/players online count` をポーリングし、失敗時は前回値を維持する。実測の応答は `Online players (0):\n`。
5. **phase の二重管理**: → conditions を正とし、phase は conditions から合成する（`internal/controller/status.go`）。
6. **`[JOIN]` / `[LEAVE]` の正確なログ形式**: **未検証のまま**。プレイヤー接続を要するため、unit テストのフィクスチャでしか確認していない。実運用でプレイヤーの出入りを確認し、必要なら `internal/agent/logs.go` の正規表現を調整すること。
7. **experimental ビルドの認証付きダウンロード**: `?username=&token=` の URL 組み立ては unit テストで検証済み。実際の認証付きダウンロードは行っていない。
8. **ゲームプロセスの異常終了**: → agent は `Agent.Fatal()` で `errUnexpectedGameExit` を報告し、`cmd/agent` が listener を閉じて非ゼロで終了する。コンテナは `restartPolicy: Always` の kubelet が再起動し、新しい agent が `agent-state.json` から復帰する（§6）。繰り返し落ちるゲームは CrashLoopBackOff になる。要求された stop は fatal 経路に入らない。README の Known limitations に記載。
9. **過去バージョンのゲームバイナリ**: ephemeral volume に残る（約 313 MB / 版）。Pod 再作成で消える。既定 2Gi では数回のバージョン変更で埋まる。
10. **GitHub Actions 上での e2e**: ローカルの kind（docker 29.9.3 / kind v0.33.0）でのみ確認済み。runner 上では未実行。Docker Hub と factorio.com への外向き通信が必要。
11. **`agent-state.json`**: save volume に最終 Config を保存するため、Discord の webhook URL と S3 の鍵が平文で入る（パーミッション 0600）。
12. **`config/` の Kustomize マニフェストと e2e の追従**: → 解消。Kustomize のツリー（`config/`）は削除し、Helm chart `charts/koujouo` が manager の設定を ConfigMap（`koujouo-config`）として書き、Deployment の `args` を `--config /etc/koujouo/manager.yaml` だけにした。`controllerImage` は chart が自身の `image` values から書くので、`KOUJOUO_CONTROLLER_IMAGE` も `config/default/manager_metrics_patch.yaml` も不要になった。`make manifests` は CRD と manager の ClusterRole を `charts/koujouo/templates/` に直接生成し、`make lint` が `helm lint charts/koujouo` を回す。サンプルは `examples/` に移した（適用順は `examples/README.md`）。e2e（`test/e2e`）も `helm upgrade --install` で chart をデプロイし、`helm uninstall` と CRD の明示削除で後始末する（§7.3）。
13. **reconcile のタイムアウトと configure / stop の待機**: 待機と進捗の status パッチは `context.WithoutCancel` で reconcile の期限から切り離してある。controller の停止時のみ中断され、次の reconcile が続きを行う。`callAgentStop` も同じ扱いで、期限で戻さない（戻すと、ゲームが動いたまま StatefulSet を消すことになる）。`agentstop_internal_test.go` がその挙動を固定している。
14. **agent Pod のネットワーク分離**: → NetworkPolicy を controller が作る（§3.3, §6）。「NetworkPolicy はユーザー側で任意設定」という当初案は取り下げた: 認証なしの平文 RPC を既定で pod ネットワークに晒すのは危険で、ユーザーに設定を忘れさせないため。`ManagerNamespace`（`POD_NAMESPACE`）が空のときだけ作らない。
15. **pod template の変更をどう反映するか**: → ハッシュ annotation（`koujouo.anqou.net/template-hash`）で判定し、controller が Pod を削除して StatefulSet に作り直させる（§3.3, §6）。`OnDelete` のままでは template の変更が動いている Pod に届かないため。image だけはハッシュから除き、インプレース更新の経路に限定する。副作用として image 以外の変更はゲームを 1 回再起動する（README の Known limitations に記載）。
16. **`spec.gameConfig` を省略したときの write-data**: → agent が既定の config.ini（`[path]\nwrite-data=<saveDir>`）を書く（§6）。省略できることを README に明記した。
17. **e2e に「gameConfig なし」「ダウンロード中の suspend」のシナリオが無い**: **未着手のまま**。前者は unit テスト（`TestWriteConfigFilesWritesADefaultConfigIni`）でカバーしたが、実ゲームでの既定 config.ini の起動は未検証。後者は `Shutdown` の期限（`TestShutdownReturnsWhenTheConfigureHoldsTheLock`）でロック競合としてカバーしたが、実クラスタでの挙動は未確認。
