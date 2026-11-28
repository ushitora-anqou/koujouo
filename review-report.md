# コードレビューレポート

## 概要

- レビュー対象範囲: `HEAD`（`b2aa14d`）のツリー全体。コミット単位の差分ではなく、現行コードを単一の変更として扱った。
- レビュー日: 2026-11-28
- 確認したこと: `go build ./...` と `go test ./internal/agent/ ./cmd/...` は成功。`internal/controller` の envtest と `test/e2e` は実行していない。指摘はコードの読み取りに基づく。
- 観点: 動作正確性、セキュリティ、可用性、運用性、テスト網羅性。

## 総評

設計は一貫しており、マネージャとエージェントの責務分離、`OnDelete` とポッドテンプレートハッシュによるインプレース更新、`context.WithoutCancel` による長時間処理の保護など、意図が読み取れる作りになっている。一方で、本番運用に進む前に直すべき問題がいくつかある。とくに次の 3 点は、サーバーが復旧不能になったり、設定変更が反映されなかったり、任意コード実行につながったりするため、優先して対処してほしい。

1. configure または resume が失敗したあと、コントローラが自動で再試行しない。
2. 参照している ConfigMap と Secret の変更が、FactorioServer の spec を触るまで反映されない。
3. エージェント RPC が無認証で、`upgrade` が任意のバイナリを `execve(2)` する。防御は NetworkPolicy だけに依存している。

## 指摘の要約

| 重要度 | 件数 | 主な内容 |
| --- | --- | --- |
| 高 | 3 | 失敗した configure の再試行なし、参照オブジェクトの変更が未反映、無認証 RPC と任意バイナリ実行 |
| 中 | 13 | 資格情報のログ漏えい、待機時間の無制限、PVC 共有と replicas の未同期、S3 リージョン、部分的なセーブファイル、suspend の膠着、Pod キャッシュ、既定 config.ini、latest タグ、バックアップ保持、イメージ更新の膠着、リソース既定値、マネージャの停止猶予 |
| 低 | 15 | ダウンロードの完全性検証、RBAC の過剰付与、CRD の再インストール、ヘルスチェック、ログ量、メトリクス、egress の全開放など |

## 修正状況（2026-11-28）

レビュー後の修正で、高の 3 件は解消した。中・低の指摘は未着手である。

| 指摘 | 状態 | 主な変更 | テスト |
| --- | --- | --- | --- |
| 高-1 configure と resume の失敗が自動再試行されない | 修正済み | `needsConfigure` が `rpcapi.Phase_failed` も再試行の条件にする | `factorioserver_controller_test.go` "should retry a configure after the agent reported a failure" |
| 高-2 参照している ConfigMap と Secret の変更が反映されない | 修正済み | 解決後の設定全体の SHA-256 を `status.observedConfigHash` に保存して比較する。参照先が無い場合は 5 秒で再試行する | "When a referenced ConfigMap or Secret changes"、"When a spec reference cannot be resolved" |
| 高-3 エージェント RPC が無認証で upgrade が任意バイナリを実行する | 修正済み | サーバーごとの RPC トークン Secret を作成して Pod にマウントし、configure / stop / upgrade の呼び出しごとに `crypto/subtle.ConstantTimeCompare` で検証する。status は秘密を運ばないため認証しない | `TestAgentRejectsAWrongToken`、`TestAgentReadsTheTokenFile`、`TestAgentWithoutATokenAcceptsEveryCall`、controller のトークン送信検証 |
| テストの不足 | 一部追加 | 上記の回帰テストと、e2e のトークン Secret 確認を追加 | e2e は 2026-11-29 に 8/8 スペック成功（トークン mount と configure / stop / upgrade の認証、旧イメージからのインプレース更新を含む） |

## テストの不足以外に関する指摘

### commit b2aa14d: docs: describe the split CI and the restarted agent

対象は上記コミットのツリー全体である。以下の行番号はこのツリーのもの。

#### [高] configure と resume の失敗が自動再試行されない

- 該当箇所: `internal/controller/factorioserver_controller.go:431`（`progressReporter`）、`internal/controller/factorioserver_controller.go:449`（`needsConfigure`）、`internal/controller/status.go:61`（`syncAgentStatus`）、`internal/controller/agentconfig.go:133`（`awaitConfigure`）、`internal/agent/agent.go:623`（`applyConfigLocked`）
- 問題:
  - `progressReporter` は configure の進捗を 2 秒ごとにポーリングし、`syncAgentStatus` を通じて `status.observedGeneration` を現在の generation に更新する。
  - `needsConfigure` は `reported.gameVersion != spec.gameVersion` と `status.observedGeneration != generation` の 2 つだけを見る。
  - エージェントはダウンロードより前に `a.config = config` を設定するため、ダウンロードやゲーム起動が失敗しても、報告する `gameVersion` は spec と一致したままになる。
  - その結果、進捗ポーリングが 1 回でも走ったあとに configure が失敗すると、次の reconcile では両方の条件が偽になり、configure が再送されない。サーバーは `Failed` のまま、ユーザーが spec を変更するまで復旧しない。
  - 同じ問題は Pod 再起動後の resume にもある。resume は 3 回で諦め、`phase failed` を報告する。コントローラはその status を `syncAgentStatus` で取り込み、`observedGeneration` を進めるため、以後 configure を送らない。
  - マネージャのロールアウト中にダウンロードが切断された場合も、この経路に入る。一時的なネットワーク障害が恒久的な停止になる。
- 修正案:
  - 失敗した phase を「設定済み」とみなさない。最小の修正は次のとおり。
    ```go
    func (r *FactorioServerReconciler) needsConfigure(
        server *koujouoanqounetv1.FactorioServer,
        reported *agentStatus,
    ) bool {
        if reported.phase == rpcapi.Phase_failed {
            return true
        }

        return reported.gameVersion != server.Spec.GameVersion ||
            server.Status.ObservedGeneration != server.Generation
    }
    ```
  - より筋の良い修正は、`ObservedGeneration` を「configure が成功した generation」の意味に限定することだ。進捗ポーリング用の status 更新では `ObservedGeneration` を進めず、configure 成功後の最終パッチだけで進める。あるいは `Configured` condition や `status.observedConfigHash` を追加し、成功時だけ更新する。
  - コントローラのテストに「configure が失敗し、phase failed を報告する」ケースを追加し、次の reconcile で configure が再度呼ばれることを確認する。e2e には、ダウンロードを 1 回失敗させて復旧するシナリオがあるとよい。

#### [高] 参照している ConfigMap と Secret の変更が反映されない

- 該当箇所: `internal/controller/factorioserver_controller.go:161`（`SetupWithManager`）、`internal/controller/factorioserver_controller.go:449`（`needsConfigure`）、`internal/controller/agentconfig.go`（Secret と ConfigMap の読み取り）
- 問題:
  - `SetupWithManager` は FactorioServer、StatefulSet、Pod だけを watch している。参照している ConfigMap と Secret は watch していない。
  - `needsConfigure` は generation と gameVersion しか見ない。ConfigMap や Secret を編集しても FactorioServer の generation は変わらない。アノテーションの変更でも変わらない。
  - Pod が再起動しても、コントローラは `needsConfigure` が偽のまま configure を送らない。エージェントは `agent-state.json` の古い設定で resume する。ゲーム設定、Discord の webhook、S3 の鍵、Factorio のダウンロード資格情報のローテーションは、すべて spec を触るまで反映されない。
  - `config.ini` はゲーム設定の主要な入力であり、これを編集しても反映されないのは機能上の大きな穴である。Secret のローテーションが効かないことは、セキュリティ運用上の問題でもある。
- 修正案:
  - 案 1: 参照オブジェクトを watch し、FactorioServer にマッピングする。
    ```go
    Watches(&corev1.ConfigMap{}, handler.EnqueueRequestsFromMapFunc(r.configMapToServers)).
    Watches(&corev1.Secret{}, handler.EnqueueRequestsFromMapFunc(r.secretToServers))
    ```
    Secret を watch するとマネージャのキャッシュに Secret が載る。現状の「Secret をキャッシュしない」設計と衝突するため、Secret は `APIReader` で読み続け、watch は ConfigMap だけにするか、Secret も含めて案 2 のハッシュ方式を選ぶ。
  - 案 2: 各 reconcile で参照先を読み直し、解決後の設定全体のハッシュを計算する。`status.observedConfigHash` などのフィールドに保存し、ハッシュが変わったら configure を送る。既存の `APIReader` を使うため、Secret をキャッシュしない設計を保てる。30 秒ごとのポーリングに相乗りできる。
  - どちらの案でも、ConfigMap と Secret を変更して reconcile したときに configure が呼ばれるテストを追加する。参照先が見つからない場合の再試行も、watch がないと指数バックオフの最大待ち（約 16 分）まで遅れる。これも合わせて解消される。

#### [高] エージェント RPC が無認証で、upgrade が任意のバイナリを実行する

- 該当箇所: `rpc/koujouo.capnp`（`upgrade`）、`internal/agent/upgrade.go`（`binReceiver`、`execInto`）、`internal/controller/resources/networkpolicy.go`（RPC の ingress 規則）
- 問題:
  - エージェントの Cap'n Proto RPC は平文で、認証がない。`configure` はゲーム設定を変更でき、`stop` はゲームを停止できる。`upgrade` は受け取ったバイナリの sha256 を送信者の値と照合したうえで `execve(2)` する。sha256 は改ざん検知ではなく、送信者が渡した値との一致を見ているだけである。
  - 防御は NetworkPolicy だけである。TCP 9550 をマネージャの namespace から許可するが、その namespace の任意の Pod を許可している。NetworkPolicy を強制しない CNI を使うクラスタや、`ManagerNamespace` が空になる構成（マネージャをクラスタ外で動かす場合）では、Pod ネットワーク全体から到達できる。
  - 影響は、エージェント Pod での任意コード実行、セーブ PVC と `agent-state.json` 内の Secret の読み取り、ゲームの停止である。マネージャ namespace に CI ランナーや他のワークロードが同居する構成では、影響が広がる。
- 修正案:
  - RPC に認証を追加する。現実的なのは、サーバーごとのランダムなトークンを Secret として FactorioServer の namespace に作り、エージェント Pod にファイルとしてマウントし、マネージャも同じ Secret を読む方式である。Cap'n Proto の bootstrap か最初の `authenticate` 呼び出しでトークンを要求する。トークンの Secret を作るために、マネージャの RBAC に Secret の create を追加する必要がある。
  - mTLS でもよいが、証明書の発行と配布の実装量が増える。
  - 併せて、NetworkPolicy の ingress を namespace 全体ではなくマネージャ Pod のラベルに限定する。`ManagerNamespace` が空のときに NetworkPolicy を作らない現在の挙動は、本番では警告だけでなくエラーにするか、明示的なオプトインにする。
  - CNI が NetworkPolicy を強制することを前提にするなら、README の前提条件に明記する。ただし、認証なしの RPC を CNI だけに任せるのは防御として弱い。

#### [中] ダウンロード資格情報がエラーログに漏れる

- 該当箇所: `internal/agent/download.go:180`（`downloadArchive`）、`internal/agent/agent.go:684`（`failLocked`）、`internal/controller/agentconfig.go:92`（`callAgentConfigure`）
- 問題:
  - `downloadURL` は experimental ビルドや古いビルドのために `?username=...&token=...` を付ける。
  - `downloadArchive` は失敗時に `endpoint` をそのままエラーへ含める。エージェントはそのエラーをログに出し、configure のエラーとしてマネージャへ返す。マネージャも受け取ったエラーをログに出す。
  - ダウンロードに失敗したとき、Factorio アカウントのトークンとユーザー名がログ基盤に残る。
- 修正案:
  - エラー用の URL を作り、クエリの資格情報を伏せる。
    ```go
    func redactDownloadURL(endpoint string) string {
        parsed, err := url.Parse(endpoint)
        if err != nil {
            return "<invalid URL>"
        }

        query := parsed.Query()
        if query.Has("username") {
            query.Set("username", "REDACTED")
        }
        if query.Has("token") {
            query.Set("token", "REDACTED")
        }

        parsed.RawQuery = query.Encode()

        return parsed.String()
    }
    ```
  - あるいはエラーには version と HTTP ステータスだけを含め、URL を含めない。
  - テストで、認証付きダウンロードの失敗メッセージにトークンが含まれないことを確認する。

#### [中] configure、stop、upgrade の待機に上限がなく、8 並列のワーカーが枯渇しうる

- 該当箇所: `internal/controller/agentconfig.go:92`（`callAgentConfigure`）、`internal/controller/agentconfig.go:133`（`awaitConfigure`）、`internal/controller/agentconfig.go:323`（`callAgentStop`）、`internal/controller/agentupgrade.go`（`pushAgentBinary`）、`internal/controller/factorioserver_controller.go:70`（`defaultMaxConcurrentReconciles`）
- 問題:
  - dial には 5 秒、status には 30 秒の上限があるが、configure、stop、upgrade には上限がない。エージェントが接続を開いたまま応答しないと、reconcile は永久に待つ。
  - `awaitConfigure` は `waitCtx := context.WithoutCancel(ctx)` を作り、`case <-waitCtx.Done()` を待つ。`WithoutCancel` の context の `Done()` は nil なので、この分岐には到達しない。コメントには「コントローラの停止時だけここに来る」とあるが、実際には来ない。RPC 自体も `context.WithoutCancel(ctx)` で開始しているため、マネージャの停止では中断されない。
  - マネージャの `MaxConcurrentReconciles` は 8 である。8 台のエージェントが応答しない、または `factorio --create` がハングすると、すべてのワーカーが埋まり、他の FactorioServer の reconcile が止まる。healthz と readyz は Ping なので、この状態でもマネージャは正常に見える。
  - `createSaveLocked` は `exec.CommandContext` に期限のない context を渡している。`--create` がハングすると configure が永久に終わらない。
- 修正案:
  - RPC 呼び出しに絶対的な上限を付ける。例として、drain 300 秒、セーブ 300 秒、ダウンロード 30 分、バックアップ 300 秒を足すと 45 分になる。余裕を見て 50 分程度にする。
    ```go
    callCtx, cancel := context.WithTimeout(r.shutdownCtx, r.agentCallTimeout())
    defer cancel()

    future, release := agent.Configure(callCtx, ...)
    ```
  - `shutdownCtx` には、reconcile の期限ではキャンセルされず、マネージャの停止ではキャンセルされる context を渡す。マネージャの停止シグナルから作った context を `FactorioServerReconciler` に持たせる。
  - `createSaveLocked` には別の短い上限（例 5 分）を付け、タイムアウト時にプロセスを kill して一時ファイルを消す。
  - タイムアウトしたら接続を解放して reconcile をエラーで返し、ワーカーを空ける。エージェント側は RPC のキャンセルでダウンロードなどを中断する。次の reconcile が再試行する。
  - テストでは、configure が返らない fake agent を使い、上限時間で reconcile が戻ること、別のサーバーの reconcile が進むことを確認する。

#### [中] 保存 PVC の共有と replicas のドリフトでデータが破壊されうる

- 該当箇所: `api/v1/factorioserver_types.go`（`savePersistentVolumeClaimName`）、`internal/controller/resources/statefulset.go:139`（`Replicas`）、`internal/controller/factorioserver_controller.go:672`（`syncStatefulSet`）
- 問題:
  - 同じ namespace の 2 つの FactorioServer が同じ PVC を指せる。コントローラは 2 つの StatefulSet を作り、両方が同じ PVC を read-write でマウントする。RWO でも同じノードにスケジュールされれば両方マウントでき、`saves/map.zip`、`config.ini`、`server-whitelist.json`、`agent-state.json` が衝突する。ワールドが壊れる。
  - エージェントは PVC のルートに `config.ini` などを書く。FactorioServer の PVC を別アプリケーションの PVC に向けると、そのアプリケーションのファイルを上書きする。API のコメントは Secret の読み取りについては警告しているが、PVC の書き込みについては触れていない。
  - `syncStatefulSet` は `spec.replicas` を同期しない。`kubectl scale statefulset --replicas=2` の結果は残り、2 つ目の Pod が同じ PVC でゲームを起動する。コントローラが所有するフィールドのはずの replicas が修復されない。
- 修正案:
  - `syncStatefulSet` で replicas を同期する。
    ```go
    if !equality.Semantic.DeepEqual(existing.Spec.Replicas, desired.Spec.Replicas) {
        existing.Spec.Replicas = desired.Spec.Replicas
    }
    ```
  - 同じ PVC を複数の FactorioServer が使えないようにする。プロジェクトには webhook がないため、最初の実装は `FactorioServer` をリストして同じ PVC を指すものが他にあれば `Failed` にする方法が考えられる。競合を確実に防ぐには validating webhook が必要である。
  - API のコメントに、PVC は専用で、エージェントがルートにファイルを書くことを明記する。`examples/` の PVC 名がサーバーごとに一意であることも説明する。
  - テストに、replicas を手で変えて reconcile で戻ること、同じ PVC を指す 2 つ目の CR が拒否または Failed になることを追加する。

#### [中] S3 のリージョンを指定できず、ホスト名からの推定が AWS のグローバルエンドポイントで誤る

- 該当箇所: `api/v1/factorioserver_types.go`（`BackupSpec`）、`internal/agent/backup.go:118`（`regionFromEndpointURL`）
- 問題:
  - SigV4 のリージョンはエンドポイントのホスト名から推定している。`https://s3.amazonaws.com` では `amazonaws` になり、AWS が期待するリージョンと一致しない。`https://s3.us-east-1.amazonaws.com` は `us-east-1` になり正しい。カスタムエンドポイントは `us-west-00` にフォールバックする。
  - S3 互換ストレージでもリージョンが署名に必要な場合があり、CRD にリージョンを指定する方法がない。AWS のグローバルエンドポイントや、リージョン名がホスト名に現れない構成でバックアップが失敗する。
- 修正案:
  - `BackupSpec` に `region` を追加し、capnp の `BackupSettings` とエージェントの `BackupSettings`、保存状態にも追加する。指定があればそれを使い、なければ現在の推定にフォールバックする。capnp は既存の番号を変えず、新しい番号でフィールドを追加する。
    ```go
    // region is the SigV4 region of the endpoint. Empty derives it from
    // endpointURL, which covers Backblaze B2 and other S3-compatible stores.
    // +optional
    Region string `json:"region,omitempty"`
    ```
  - `examples/factorioserver.yaml` に例を追加する。
  - `TestRegionFromEndpointURL` に `https://s3.amazonaws.com` と、リージョンを明示した場合のテストを追加する。

#### [中] 中断された `--create` が壊れた map.zip を残し、以後 CrashLoop になる

- 該当箇所: `internal/agent/agent.go:778`（`ensureSaveLocked`）、`internal/agent/agent.go:814`（`createSaveLocked`）
- 問題:
  - 新しいセーブの作成は `factorio --create <saves/map.zip>` を直接実行する。`exec.CommandContext` の context は configure 呼び出しのもので、期限がない。マネージャの切断や Pod の終了で `--create` が途中で止まると、部分的に書かれたファイルが残る。
  - 作成の途中で kill されると、`map.zip` が部分的に残る。`createSaveLocked` のエラー経路もこのファイルを消さない。
  - 次回の configure では `ensureSaveLocked` がファイルの存在だけを確認して作成をスキップする。Factorio は壊れたセーブを読み込めずに終了し、エージェントはゲームの異常終了として Pod を再起動する。resume も同じファイルを使うため、CrashLoopBackOff になる。
  - 初回セットアップ中のマネージャのロールアウトやネットワーク断で起こりうる。復旧には手動で `map.zip` を消す必要がある。
- 修正案:
  - 同じ PVC 上の一時ディレクトリに作成し、成功後に rename する。
    ```go
    temporaryDir := filepath.Join(a.savesDir(), ".create-"+randomSuffix())
    temporarySave := filepath.Join(temporaryDir, saveFileName)

    // factorio --create temporarySave

    if err != nil {
        _ = os.RemoveAll(temporaryDir)
        return err
    }

    if err := os.Rename(temporarySave, savePath); err != nil {
        _ = os.RemoveAll(temporaryDir)
        return err
    }

    _ = os.RemoveAll(temporaryDir)
    ```
  - `ensureSaveLocked` では、ファイルサイズが 0 でないこと、ZIP の先頭が `PK\x03\x04` であること程度を確認する。壊れていれば削除して作り直す。
  - `--create` に短いタイムアウトを付ける（前述の待機時間の上限とは別に、作成だけの上限）。
  - 部分的なファイルを残す fake バイナリを使ったテストを追加する。

#### [中] suspend がエージェントに到達できないと永久に失敗する

- 該当箇所: `internal/controller/factorioserver_controller.go:345`（`reconcileSuspended`）、`internal/controller/factorioserver_controller.go:396`（`stopAgentOfPod`）
- 問題:
  - suspend 時、Pod が Ready なら `stopAgentOfPod` を呼ぶ。dial に失敗するとエラーを返し、StatefulSet を削除せずに reconcile を終える。
  - エージェントが NetworkPolicy の設定ミス、ハング、クラッシュで応答しない場合、suspend は永久に失敗する。ユーザーはサーバーを停止できない。
  - StatefulSet を削除すれば Pod に SIGTERM が届き、エージェントは保存して終了する。Pod が Ready でないときに stop をスキップして StatefulSet を削除する経路はすでにあり、同じフォールバックを使える。
- 修正案:
  - 一定回数または一定時間 stop に失敗したら、StatefulSet を削除して SIGTERM に任せる。スキップしたことを condition と event で報告する。
  - 例として、`status` の condition か annotation に失敗回数を記録し、3 回失敗したら削除する。あるいは `spec` に明示的な強制停止のフィールドを追加する。
  - テストに、dial が失敗し続ける場合に StatefulSet が削除されることを追加する。現在のテストは「StatefulSet が残る」ことを固定しているため、意図を再検討する必要がある。

#### [中] マネージャがクラスタ全体の Pod と StatefulSet をキャッシュする

- 該当箇所: `internal/controller/factorioserver_controller.go:161`（`SetupWithManager`）、`cmd/manager/main.go`（`ctrl.NewManager` のオプション）
- 問題:
  - `Watches(&corev1.Pod{}, ...)` と `Owns(&appsv1.StatefulSet{}, ...)` により、controller-runtime の既定のキャッシュはクラスタ内のすべての Pod と StatefulSet を watch してキャッシュする。ラベルの predicate はキャッシュの後段で効くため、メモリと API の watch 負荷はすべてのオブジェクトに比例する。
  - 大規模クラスタでは、operator が所有しない Pod の分だけメモリを消費する。`findAgentPod` はキャッシュ全体からラベルで絞り込む。
- 修正案:
  - `cache.Options.ByObject` で Pod と StatefulSet をラベルで絞る。
    ```go
    requirement, err := labels.NewRequirement(
        resources.FactorioServerLabelKey, selection.Exists, nil)
    if err != nil {
        return err
    }
    selector := labels.NewSelector().Add(*requirement)

    cacheOptions := cache.Options{
        ByObject: map[client.Object]cache.ByObject{
            &corev1.Pod{}:         {Label: selector},
            &appsv1.StatefulSet{}: {Label: selector},
        },
    }
    ```
    `ctrl.NewManager` の `Cache` に渡す。`FactorioServer` は全件必要なのでそのまま。
  - テストでは、ラベルのない Pod がキャッシュに載らないことまでは確認しにくいが、ラベルのある Pod で reconcile が動くことを確認する。

#### [中] 既定の config.ini に read-data がなく、gameConfig 省略時の起動が未検証

- 該当箇所: `internal/agent/agent.go:698`（`writeConfigFilesLocked`）、`docs/design.md` の 8.17
- 問題:
  - `spec.gameConfig` を省略した場合、エージェントは `[path]` と `write-data` だけの config.ini を書く。
  - Factorio の配布物の config.ini は `read-data=__PATH__executable__/../../data` も設定している。これがない場合にゲームがデータディレクトリを見つけられるかは、実ゲームで確認されていない。design.md の 8.17 も未検証と認めている。README は「必須フィールドだけの FactorioServer が起動する」と案内している。
- 修正案:
  - 既定の config.ini に `read-data` も書く。
    ```ini
    [path]
    read-data=__PATH__executable__/../../data
    write-data=<saveDir>
    ```
  - `spec.gameConfig` なしの e2e シナリオを追加し、Running になりゲームがホストされることを確認する。Factorio が read-data を既定値で補うなら、その旨を design.md と README に書き、最小の config.ini を維持してよい。

#### [中] イメージタグ latest と imagePullPolicy IfNotPresent の組み合わせ

- 該当箇所: `charts/koujouo/values.yaml:24`（`tag: latest`）、`charts/koujouo/values.yaml:25`（`pullPolicy: IfNotPresent`）、`charts/koujouo/templates/deployment.yaml`
- 問題:
  - chart の既定は `image.tag: latest` と `image.pullPolicy: IfNotPresent` である。ノードに古い `latest` がキャッシュされていると、新しい master のイメージを pull しない。
  - master への push で `latest` が更新されても、Pod テンプレートの image 文字列は変わらない。chart 自体が変わらなければ `helm upgrade` でも Deployment はロールアウトしない。マネージャが古いイメージのまま残る。
  - エージェント Pod は `imagePullPolicy` を明示していないため、`latest` では既定で Always になる。マネージャだけが IfNotPresent で取り残される。
- 修正案:
  - 既定を不変タグ（リリースの `vX.Y.Z`）にし、README と values のコメントで `--set image.tag=<tag>` を必須に近い形で案内する。
  - `latest` を許すなら、そのときだけ `pullPolicy: Always` にする。Helm のテンプレートで条件分岐できる。
    ```yaml
    imagePullPolicy: {{ if eq .Values.image.tag "latest" }}Always{{ else }}{{ .Values.image.pullPolicy }}{{ end }}
    ```
  - `latest` と IfNotPresent の組み合わせを lint で検出するのは難しいため、values のコメントを強化する。

#### [中] バックアップの保持期間がなく、オブジェクトが無制限に増える

- 該当箇所: `internal/agent/backup.go:30`（バックアップの定数）、`api/v1/factorioserver_types.go`（`BackupSpec` のコメント）
- 問題:
  - バックアップは起動直後、1 時間ごと、停止時に 1 回アップロードされる。古いオブジェクトは削除されない。API のコメントにも「Old objects are never deleted」とある。
  - セーブが数十 MB だと、1 台あたり年間で数百 GB になる。バケットのライフサイクル規則をユーザーが設定しない限り、ストレージ費用が増え続ける。README にはこの運用要件がない。
- 修正案:
  - `BackupSpec` に保持数または保持期間を追加し、アップロード後に同じ prefix の古いオブジェクトを削除する。S3 の ListObjectsV2 と DeleteObject が必要になる。
  - 実装しない場合は、README と API のコメントに、バケット側のライフサイクル規則が必須であることを明記する。少なくとも、既定の運用として案内する。

#### [中] イメージだけの更新は、既存エージェントが RPC に応答できないと詰まる

- 該当箇所: `internal/controller/agentupgrade.go`（`needsAgentUpgrade`、`upgradeAgent`）、`internal/controller/resources/statefulset.go`（`podTemplateHash` は image を除外）
- 問題:
  - ポッドテンプレートのハッシュは image を除外している。image だけの変更では Pod は再作成されず、新しいマネージャが RPC でエージェントのバイナリを置き換える。
  - 古いエージェントがクラッシュループやハングで RPC に応答できない場合、マネージャはアップグレードできない。Pod は同じ古いイメージで再起動し続け、自動では復旧しない。手動で Pod を削除するまで続く。
  - README の既知の制限はゲームのクラッシュについて説明しているが、エージェント自体が応答しない場合の復旧方法は書かれていない。
- 修正案:
  - 一定時間（例 10 分）エージェントに到達できず、かつ希望するイメージが Pod のイメージと異なる場合、Pod を削除して StatefulSet に新しいイメージで再作成させる。ゲームは 1 回再起動するが、自動復旧を優先する。event で理由を報告する。
  - あるいは README に `kubectl delete pod <server>-0` の手順を明記する。自動化するなら、`status` に `AgentUnavailable` の持続時間を記録し、それを見て判断する。
  - コントローラのテストに、dial が失敗し続け、`ControllerImage` が Pod のイメージと異なる場合の Pod 削除を追加する。

#### [中] ゲームに既定の CPU とメモリの requests がない

- 該当箇所: `internal/controller/resources/statefulset.go:222`（`agentContainer`）、`internal/controller/resources/statefulset.go:270`（`spec.agent.resources`）
- 問題:
  - `spec.agent.resources` を省略すると、コンテナの resources は空になる。Factorio はノードの CPU とメモリを無制限に使える。1 台のゲームがノードを圧迫し、他の Pod を退避させたり、ノードの OOM を引き起こしたりする。
  - マネージャの Deployment も `resources: {}` が既定である。
- 修正案:
  - エージェントのコンテナに控えめな requests を既定として設定する（例 CPU 500m、メモリ 1Gi）。limits はゲームの OOM を招くため、既定では設定せず、`spec.agent.resources` でユーザーが指定できるようにする。メモリの limit を設定する場合は、ゲームの使用量を実測したうえで余裕を持たせる。
  - `ephemeral-storage` の requests も設定し、ゲームバイナリの容量（1 バージョン約 313 MB）を考慮する。
  - README に、実運用では `spec.agent.resources` を設定することを勧める。マネージャにも requests の既定を設定する。

#### [中] マネージャの terminationGracePeriodSeconds が 10 秒で、長時間の reconcile を切る

- 該当箇所: `charts/koujouo/templates/deployment.yaml:106`（`terminationGracePeriodSeconds: 10`）、`cmd/manager/main.go`（`LeaderElectionReleaseOnCancel` はコメントアウト）
- 問題:
  - マネージャのロールアウトで SIGTERM が届くと、reconcile の context は `WithoutCancel` で切り離されているため configure や stop は中断されない。しかし Pod は 10 秒で kill される。TCP 接続が切れ、エージェント側の configure も中断される。
  - 中断された configure が失敗した状態で残ると、前述の再試行の問題と組み合わさってサーバーが復旧しなくなる。リーダー選出の解放も無効なので、新しいマネージャはリースの期限まで待つ。
- 修正案:
  - マネージャの `terminationGracePeriodSeconds` を 30 秒から 60 秒程度に延ばし、status 呼び出しや短い reconcile が終わる余裕を作る。長時間の configure は別途上限を設ける（前述）。
  - `LeaderElectionReleaseOnCancel` を有効にする場合は、マネージャが停止後すぐに終了することを確認する。現在の構成では有効にしてよいはずだが、クリーンアップ処理を足すなら注意する。
  - テストでは、SIGTERM 相当のキャンセルで reconcile がどう振る舞うかを確認する。

#### [低] ゲームバイナリのダウンロードに完全性検証がない

- 該当箇所: `internal/agent/download.go`
- 問題: アーカイブは HTTPS で取得されるだけで、チェックサムや署名の検証がない。CDN や TLS の経路が侵害されると、エージェントの権限で任意のコードが実行される。エージェント自身のアップグレードは sha256 を検証しているが、ゲームのダウンロードは検証していない。
- 修正案: Factorio が公開するハッシュがあれば固定し、取得後に検証する。ない場合は、展開したバイナリの sha256 をログに残し、起動に失敗したディレクトリを削除して再ダウンロードする。残存リスクを README に書く。

#### [低] マネージャの ClusterRole が Secret の list/watch を許可している

- 該当箇所: `charts/koujouo/templates/manager-role.yaml`（`internal/controller/factorioserver_controller.go` の RBAC マーカーから生成）、`internal/controller/agentconfig.go`（`APIReader` の `Get` のみ）
- 問題: コントローラは Secret を `Get` でしか読まないが、ClusterRole は `get`、`list`、`watch` を許可している。Secret をキャッシュしない設計と合わせると、`list` と `watch` は不要である。
- 修正案: RBAC マーカーを `get` だけに絞る。参照オブジェクトの watch を追加する場合は、その時点で必要な権限だけを足す。

#### [低] `fullnameOverride` によりクラスタスコープの名前がリリース間で衝突する

- 該当箇所: `charts/koujouo/values.yaml`（`fullnameOverride: koujouo`）、`charts/koujouo/templates/manager-role.yaml` などの ClusterRole
- 問題: 同じ chart を別の namespace に 2 回インストールすると、ClusterRole と ClusterRoleBinding の名前が同じになる。片方を `helm uninstall` すると共有の ClusterRole が消え、もう片方のマネージャが権限を失う。CRD も共有である。
- 修正案: クラスタスコープの名前をリリースごとに一意にするか、1 クラスタ 1 リリースであることを README に明記する。

#### [低] CRD の keep ポリシーで再インストールが失敗し、CRD 削除で全 CR が消える

- 該当箇所: `charts/koujouo/templates/koujouo.anqou.net_factorioservers.yaml`、README
- 問題: CRD は `helm.sh/resource-policy: keep` を持つため `helm uninstall` では残る。残った CRD の上に `helm install` すると、Helm が所有権のない CRD を扱えず失敗する（e2e はインストール前に CRD を消している）。CRD を消すと、すべての FactorioServer が削除される。
- 修正案: 再インストールの手順を README と chart のコメントに目立つ形で書く。可能なら、インストール前に CRD の有無を確認する pre-install フックか、`crds/` ディレクトリの利用を検討する。

#### [低] liveness が RPC の TCP 接続だけで、ゲームのハングを検知しない

- 該当箇所: `internal/controller/resources/statefulset.go`（liveness probe）、`internal/agent/agent.go`（`Status`）
- 問題: エージェントの RPC はゲームがハングしても応答し続けるため、liveness probe は通る。Pod は Ready のままで、プレイヤーは応答しないサーバーに接続する。readiness probe もないため、起動前の Pod が Service のエンドポイントになる。
- 修正案: エージェントにゲームの状態を返す小さな HTTP エンドポイントを追加し、readiness をゲームの起動状態に連動させる。liveness は RPC ハンドラの応答時間も見る。あるいは、この制約を README に明記する。

#### [低] ゲーム出力の全行を Info で記録する

- 該当箇所: `internal/agent/logs.go`（`copyGameOutput`）
- 問題: プレイヤーが多いサーバーや、ログを多く出す MOD では、エージェントのログ量が大きくなる。ログ基盤のコストと検索性に影響する。
- 修正案: `log.gameOutput` のような設定を追加し、ゲーム出力のレベルを変えられるようにする。少なくとも README にログ量の注意を書く。1 MiB の行上限と読み取りの継続は適切である。

#### [低] Discord 通知のキューが満杯だと通知を落とす

- 該当箇所: `internal/agent/logs.go`（`gameOutputQueueLength`）
- 問題: webhook が遅い、または失敗すると、キュー（64 件）が満杯になり、通知を落とす。ログには「Dropped a Discord notification」と出るが、README には記載がない。
- 修正案: 仕様として README に書く。設定でキュー長を変えられるようにする。通知の失敗を status かメトリクスで見えるようにする。

#### [低] `EndpointURL` が `http` を許可している

- 該当箇所: `api/v1/factorioserver_types.go`（`EndpointURL` の `Pattern`）
- 問題: `http://` のエンドポイントを指定すると、S3 のアクセスキーとシークレット、セーブデータが平文で流れる。内部の MinIO などでは HTTP を使う場合があり、意図的な許可とも読める。
- 修正案: 既定では `https` を要求し、内部エンドポイント向けに明示的な `insecure` フラグを用意する。少なくとも、HTTP のリスクを API のコメントと README に書く。

#### [低] メトリクスが既定で自己署名証明書で、ServiceMonitor が検証をスキップする

- 該当箇所: `charts/koujouo/values.yaml`（`serviceMonitor.insecureSkipVerify: true`）、`cmd/manager/main.go`（metrics の証明書パスは空が既定）
- 問題: controller-runtime が自己署名証明書を生成し、ServiceMonitor は検証をスキップする。開発には便利だが、本番では証明書の出所を検証できない。
- 修正案: 本番向けの values 例で `metricsCertPath` と cert-manager の Certificate を示し、`insecureSkipVerify` を false にする。

#### [低] FactorioServer の状態に対するメトリクスとアラートがない

- 該当箇所: `cmd/manager/main.go`（metrics）、`internal/controller/status.go`
- 問題: 公開されるのは controller-runtime のメトリクスだけである。`Failed` の phase、バックアップの失敗、エージェントのクラッシュループは、status とログを見ないと分からない。
- 修正案: `koujouo_factorioserver_phase`、`koujouo_agent_unavailable_total`、`koujouo_backup_failure_total` などのカスタムメトリクスを追加し、PrometheusRule の例を用意する。

#### [低] flake の dirty 版のバージョンにタイムスタンプがない

- 該当箇所: `flake.nix`（`buildVersion` は `shortRev` か `dirtyShortRev`）、`Makefile`（dirty 時にタイムスタンプを付ける）
- 問題: 同じコミットの dirty なツリーを 2 回ビルドすると、バージョンが同じになる。マネージャは新しいエージェントを自分のビルドとみなし、インプレース更新をスキップする。Nix は git 管理下のファイルだけをビルドするため通常は `git add` が前提だが、ローカルのイメージでは起こりうる。開発専用の問題である。
- 修正案: flake の `buildVersion` にも、Makefile と同じタイムスタンプかソースのハッシュを付ける。

#### [低] リリースの chart ジョブが image の merge を待たない

- 該当箇所: `.github/workflows/release.yml`（`chart: needs: [meta]`）
- 問題: `build` や `merge` が失敗しても、chart ジョブは GitHub Release を作れる。Release が存在しないイメージタグを指す状態になりうる。ワークフローは赤になるが、Release は公開されたままである。
- 修正案: `chart` を `merge` に依存させる。イメージの公開が失敗したら chart も出さない方が安全である。あるいは、Release の本文を後から更新する運用にする。現在のコメントはこのトレードオフを認識している。

#### [低] Status が失敗時にも設定上の gameVersion を返す

- 該当箇所: `internal/agent/agent.go`（`gameVersionLocked`）、`internal/controller/status.go`
- 問題: エージェントは、ゲームの起動に失敗しても設定上の `gameVersion` を報告する。`kubectl get factorioserver` の Version 列には、実際には動いていないバージョンが出る。phase は `Failed` なので区別はできるが、列だけを見ると誤解しやすい。
- 修正案: 実際に動いているゲームのバージョンを報告するか、Version 列の表示を Running のときだけにする。あるいは condition の message に失敗を明記する。

#### [低] エージェント RPC の gameVersion に入力検証がない

- 該当箇所: `internal/agent/download.go`（`gameDir`）、`internal/agent/agent.go`（`readRPCConfig`）
- 問題: CRD は `gameVersion` を正規表現で検証しているが、エージェントは RPC で受け取った値をそのままパスに使う。RPC を直接呼べる者は、`../` を含む値でキャッシュディレクトリの外を指せる可能性がある。RPC を呼べる時点で `upgrade` も呼べるため深刻度は低いが、防御を重ねる価値はある。
- 修正案: エージェント側でも `^[0-9]+\.[0-9]+\.[0-9]+$` を検証し、不正な値は拒否する。併せて、`gameDir` がキャッシュディレクトリの下に収まることを確認する。

#### [低] エージェントの egress が全開放されている

- 該当箇所: `internal/controller/resources/networkpolicy.go`（`PolicyTypes` は Ingress のみ）
- 問題: エージェントはゲームのダウンロード、S3 へのアップロード、Discord への投稿のために外向き通信を必要とする。NetworkPolicy は ingress だけを制限し、egress は制限していない。ゲームや MOD、エージェントが侵害されると、セーブデータや `agent-state.json` の Secret を任意の宛先へ送れる。
- 修正案: 標準の NetworkPolicy は FQDN を指定できないため、Cilium の FQDN ポリシーや固定 IP の egress ゲートウェイを使って、factorio.com、S3 のエンドポイント、Discord、DNS だけを許可する。すべてのユーザーに同じ宛先を強制できない場合は、egress 制限をオプトインにし、README と API のコメントに全開放であることを明記する。

## テストの不足に関する指摘

### commit b2aa14d: docs: describe the split CI and the restarted agent

#### [高] configure 失敗後の再試行を確認するテストがない

- 足りていないテスト: configure が進捗を報告したあとに失敗し、`phase failed` になるケース。fake agent は、コントローラの進捗ポーリングが `observedGeneration` を更新するまで configure を保持してから失敗させる。そうしないと、修正前のコードでもテストが通ってしまい、回帰を検出できない。次の reconcile で configure が再送されることを確認する。
- 追加すべきテストの例:
  ```go
  It("should retry a configure that reported a failure", func() {
      _, err := reconcileOnce(ctx, reconciler, serverName)
      Expect(err).NotTo(HaveOccurred())

      fakeAgent.setOnConfigure(func(f *fakeAgentServer, _ *recordedConfig) {
          f.setPhase(rpcapi.Phase_downloading)
          f.failNextConfigure(errFakeDownload)
      })

      updateServerSpec(ctx, serverName, func(spec *koujouoanqounetv1.FactorioServerSpec) {
          spec.GameVersion = testNextGameVersion
      })

      _, err = reconcileOnce(ctx, reconciler, serverName)
      Expect(err).To(HaveOccurred())

      _, err = reconcileOnce(ctx, reconciler, serverName)
      Expect(err).NotTo(HaveOccurred())
      Expect(fakeAgent.configurations()).To(HaveLen(3))
  })
  ```
  fake agent に configure の失敗注入を追加する必要がある。

#### [高] ConfigMap と Secret の変更が反映されることを確認するテストがない

- 足りていないテスト: 参照している ConfigMap の `config.ini` を変更したとき、または Secret の webhook URL を変更したときに、configure が呼ばれること。
- 追加すべきテストの例:
  ```go
  It("should configure again when the referenced ConfigMap changes", func() {
      _, err := reconcileOnce(ctx, reconciler, serverName)
      Expect(err).NotTo(HaveOccurred())
      Expect(fakeAgent.configurations()).To(HaveLen(1))

      configMap := &corev1.ConfigMap{}
      Expect(k8sClient.Get(ctx, configMapKey, configMap)).To(Succeed())
      configMap.Data["config.ini"] = "[path]\nwrite-data=/var/lib/koujouo\n"
      Expect(k8sClient.Update(ctx, configMap)).To(Succeed())

      _, err = reconcileOnce(ctx, reconciler, serverName)
      Expect(err).NotTo(HaveOccurred())
      Expect(fakeAgent.configurations()).To(HaveLen(2))
  })
  ```
  Secret の変更も同様に確認する。watch を使う場合は envtest で watch のイベントが届くことまで確認する。

#### [中] 応答しないエージェントで待機が上限に達することを確認するテストがない

- 足りていないテスト: configure と stop が返らない fake agent を使い、上限時間で reconcile が戻ること、ワーカーが解放されること。
- 追加すべきテストの例: fake agent の configure を `<-ctx.Done()` で止め、`agentCallTimeout` を短く設定して、`reconcileOnce` がタイムアウトのエラーで戻ることを確認する。その後、別の FactorioServer の reconcile が進むことも確認する。

#### [中] suspend のフォールバックを確認するテストがない

- 足りていないテスト: dial が失敗し続ける場合に、一定回数後に StatefulSet が削除されること。現在のテストは StatefulSet が残ることを固定しているため、意図を変えるならテストも変える。
- 追加すべきテストの例: `failingDialer` で 3 回 reconcile し、3 回目に StatefulSet が消えること、condition に強制停止の理由が残ることを確認する。

#### [中] replicas のドリフトと PVC 共有を確認するテストがない

- 足りていないテスト: StatefulSet の replicas を手で 0 や 2 に変えたあと、reconcile で 1 に戻ること。同じ PVC を指す 2 つ目の FactorioServer が拒否されるか、Failed になること。
- 追加すべきテストの例:
  ```go
  It("should restore the replica count", func() {
      _, err := reconcileOnce(ctx, reconciler, serverName)
      Expect(err).NotTo(HaveOccurred())

      statefulSet := agentStatefulSet()
      statefulSet.Spec.Replicas = new(int32(2))
      Expect(k8sClient.Update(ctx, statefulSet)).To(Succeed())

      _, err = reconcileOnce(ctx, reconciler, serverName)
      Expect(err).NotTo(HaveOccurred())
      Expect(agentStatefulSet().Spec.Replicas).To(HaveValue(BeEquivalentTo(int32(1))))
  })
  ```

#### [中] 中断されたセーブ作成を確認するテストがない

- 足りていないテスト: `--create` が途中で失敗したときに `map.zip` が残らないこと、次回の configure で作り直されること。
- 追加すべきテストの例: fake のゲームバイナリが `map.zip` に数バイト書いて非ゼロで終了するようにし、`createSaveLocked` のあとに `map.zip` が存在しないこと、または不完全なファイルが削除されることを確認する。

#### [中] gameConfig を省略した実ゲームの起動を確認するテストがない

- 足りていないテスト: `spec.gameConfig` を省略した FactorioServer が e2e で Running になり、ゲームがホストされること。design.md の 8.17 が未着手としている。
- 追加すべきテストの例: e2e に `newFactorioServerWithoutGameConfig` を追加し、`expectRunning` と `expectGameHosted` を確認する。

#### [中] NetworkPolicy の強制を確認するテストがない

- 足りていないテスト: e2e はマネージャ namespace からしかエージェントに接続せず、別の namespace の Pod から TCP 9550 に接続できないことを確認していない。NetworkPolicy の強制は CNI に依存するため、強制する CNI のクラスタで禁止方向の接続を試すテストが必要である。少なくとも NetworkPolicy の生成内容は unit テストで確認する。
- 追加すべきテストの例: マネージャ namespace 以外の Pod から TCP 9550 に接続できないことを、NetworkPolicy を強制する CNI のクラスタで確認する。CI の時間が増えるため、nightly の別ジョブでもよい。

#### [中] マネージャ停止中の reconcile を確認するテストがない

- 足りていないテスト: `context.WithoutCancel` により、`awaitConfigure` の `waitCtx.Done()` 分岐が到達不能であること。マネージャの停止で configure が中断されないことを確認する。
- 追加すべきテストの例: キャンセル可能な context を渡して reconcile を開始し、context をキャンセルしたあとの挙動を確認する。修正後は、停止用の context でキャンセルされること、reconcile の期限ではキャンセルされないことを確認する。

#### [中] ダウンロード資格情報の秘匿を確認するテストがない

- 足りていないテスト: 認証付きダウンロードが失敗したときのエラーメッセージに `token` と `username` の値が含まれないこと。
- 追加すべきテストの例: `TestEnsureBinaryReportsADownloadFailure` を認証付きにし、返ってきたエラーの文字列に `tok-en` が含まれないことを確認する。

#### [低] 競合検査（`go test -race`）が CI にない

- 足りていないテスト: エージェントは status、ゲーム出力の読み取り、バックアップ、アップグレードで goroutine を並行に動かす。`go test -race` を `make test` か CI の別ジョブで実行する。
- 追加すべきテストの例: `go test -race ./internal/... ./cmd/...` を CI に追加する。envtest を含めると時間がかかるため、まずは agent と cmd だけでもよい。

#### [低] S3 の AWS グローバルエンドポイントを確認するテストがない

- 足りていないテスト: `regionFromEndpointURL("https://s3.amazonaws.com")` の期待値。リージョンを明示できるようにしたあとは、指定が優先されること。
- 追加すべきテストの例: 既存の `TestRegionFromEndpointURL` にケースを追加する。

#### [低] 複数の Pod がある場合の `findAgentPod` を確認するテストがない

- 足りていないテスト: replicas のドリフトで Pod が 2 つになったとき、どちらを選ぶか、replicas を戻したあとに正しい Pod に dial するか。
- 追加すべきテストの例: ラベルの同じ Pod を 2 つ作り、`findAgentPod` の選択と reconcile の結果を確認する。

#### [低] エージェント RPC の入力検証を確認するテストがない

- 足りていないテスト: `configure` で `gameVersion` に `../../x` のような値を渡したとき、キャッシュディレクトリの外に書かないこと。CRD の正規表現でコントローラからは防げるが、RPC を直接呼べる場合の防御が薄い。
- 追加すべきテストの例: `gameBinaryPath` と `gameDir` に不正な version を渡したときのエラー、または `configure` が拒否することを確認する。

## 既知の制約として扱われているもの

以下は README や design.md に記載があり、今回の指摘には含めていない。ただし本番の脅威モデルに照らして再確認する価値がある。

- RCON のパスワードがゲームのコマンドライン引数に載り、同じ Pod の別プロセスから `/proc/<pid>/cmdline` で読める（README の既知の制限）。
- `agent-state.json` に Discord の webhook、S3 の鍵、Factorio の資格情報が平文で入る（API のコメントと design.md）。
- `[JOIN]` と `[LEAVE]` のログ形式が実マルチプレイで未検証（design.md の 8.6）。
- ゲームバイナリが ephemeral volume に版ごとに残り、既定 2Gi では数回のバージョン変更で埋まる（README）。
- ゲームの異常終了は Pod の再起動と CrashLoopBackOff につながる（README）。
- イメージ以外の Pod テンプレートの変更はゲームを 1 回再起動する（README）。

## 参考

- レビュー時点の動作確認: `go build ./...`、`go test ./internal/agent/ ./cmd/...` は成功。envtest と e2e は未実行だった。
- 修正後の最終ゲート: `make lint`、`make test`（gitleaks、envtest、unit）、`go test -tags=e2e -run '^$' ./test/e2e/`、`make test-e2e`（8/8 スペック）が成功した。
- 中・低の指摘は未着手である。高の修正で追加したテストは「修正状況」の表に示した。
