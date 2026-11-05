# notify-relay

業務アプリから配信依頼を受け、メールを並行送信するGoの内部サービス(OSS)。
API仕様の正本は api/openapi.yaml、機能仕様は specs/。

## コマンド

- テスト:`go test -race ./...`
- lint:`golangci-lint run ./...`
- 起動:`go run ./cmd/notify-relay`

## 設計ルール

- 層:`internal/domain`(型とルール、他に依存しない)→ `internal/usecase`(受付・送信・再送・結果通知、必要なインタフェースを自分で定義)→ `internal/adapter`(HTTP・SMTP・webhook)/ `internal/infra`(DB・流量制御・トレース)
- 業務ロジック(誰に何を送るか)を持たない。文面は受け取ったものを使う
- すべての配信依頼に重複防止キー。同じキー・同じ内容は200、異なる内容は409
- 宛先のメールアドレスは最終状態になったら削除し、ログには出さない(ハッシュの先頭のみ)
- 外部ライブラリは必要最小限。追加するときはADR(docs/adr/)に理由を書く

## テスト

- テーブル駆動テストで書く
- 受け入れ基準のID(例:AC-R001-10)をテスト名かサブテスト名に入れる
- 並行処理は `-race` で検証する
