# ADR-0004: トレースとメトリクスに OpenTelemetry の Go SDK を使う

- ステータス: 承認
- 日付: 2026-11-19
- 決定者: 小田 裕貴
- 関連: team-ops ADR-0026(トレースとメトリクスの方法)、team-ops specs/010-observability、AC-R001-24・25、CLAUDE.md「外部ライブラリは必要最小限」

## コンテキスト

- AC-R001-24 は、受付数、送信数(結果別)、未送信の宛先数、送信の所要時間、webhook の失敗数をメトリクスとして出すことを求める
- AC-R001-25 は、受付 → 送信 → webhook を同じトレースIDで追えることを求める。これまでは traceparent を保存して webhook に付け直すだけで、スパンを作っていなかった
- team-ops の受け口(ローカルの otel-lgtm、デモ環境の Alloy、その先の Grafana Cloud)は OTLP で受け取る(team-ops ADR-0026)
- 標準ライブラリには、スパンを作って OTLP で送る仕組みがない
- 外部ライブラリは必要最小限にし、追加するときは理由をADRに書く

## 検討した選択肢

| 案 | 内容 | 評価 |
|---|---|---|
| **A** | **OpenTelemetry の Go SDK(トレースとメトリクス)と、OTLP の HTTP の送り口** | **業界の標準で、team-ops の受け口にそのまま送れる。W3C の traceparent の解釈もそろっている。依存は増える(go.mod に十数個、送り口が使う gRPC と protobuf を含む)** |
| B | メトリクスだけ Prometheus のクライアントで出し、トレースは traceparent をログに書く | 依存は少ないが、AC-R001-25 と team-ops の AC-010-02(1本のトレースでたどる)を満たせない |
| C | OTLP の送り口を自作する | 依存は増えないが、OTLP の形式と再送を自分で保守することになり、現実的でない |

## 決定

案Aを採用する(team-ops の判断2、2026-11-19)。

- 入れるのは、otel の API と SDK(trace・metric)、`otlptracehttp`、`otlpmetrichttp` だけ。HTTP の自動計装(contrib の otelhttp)は入れず、サーバーのスパンは `internal/infra/telemetry` に小さなミドルウェアとして書く(記録するのはメソッド、ルート、状態コードだけで、URL やクエリは記録しないことを自分のコードで保証するため)
- usecase は OpenTelemetry に依存しない。usecase が `Telemetry` インタフェースを定義し、`internal/infra/telemetry` がそれを実装する(層のルール)
- 送り先は標準の環境変数(`OTEL_EXPORTER_OTLP_ENDPOINT` など)で決める。未設定か `OTEL_SDK_DISABLED=true` のときは何も送らない。そのときも、受け取った traceparent は今までどおり保存して webhook に付ける
- 配信依頼に保存する traceparent は、受付のスパンのものにする。送信と webhook のスパンはその子になり、業務アプリから見て1本のトレースになる。送信の再試行も、保存した traceparent から始めるので同じトレースに入る
- スパンに入れるのは、配信ID、宛先の行のID、試行回数、結果、エラーコードだけ。メールアドレス、email_ref、件名、本文、リンクは入れない。メトリクスのラベルは result、error_code、reason の3つだけ(値はいずれも決まった少数)

## 結果

良くなること:

- 業務アプリの公開から送信、webhook までを Grafana の1本のトレースで見られる
- 失敗の増え方や送信の遅さを、ログを数えずにメトリクスで見られる

悪くなること・受け入れるコスト:

- 依存が増え、govulncheck と Dependabot で追う対象が増える。送り口が gRPC と protobuf を間接的に持ち込む
- ローカルの開発環境では Go のモジュールを取得できないため、go.sum は CI で作った(ai-log に記録)

## 見直す条件

- OpenTelemetry の Go SDK で、送り口が gRPC に依存しない形が出た場合(依存を減らせる)
- 依存の脆弱性への対応が頻繁に必要になった場合
