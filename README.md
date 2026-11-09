# notify-relay

業務アプリから配信依頼を受け取り、メールを並行送信するGo製の内部サービスです。

> **現在の状況(2026年11月):team-ops の第1弾(001〜007)で使う機能が完成**
> 配信依頼の受付(署名検証・重複防止)、SMTP での並行送信(同時に送る数の上限つき)、一時エラーの再送、結果の webhook(まとめ送りと再送)、照会 API、保持期間を過ぎたデータの削除を実装済みです。
> システム全体の要件・設計判断と、ローカル環境の起動方法は [team-ops](https://github.com/yuukioda999/team-ops) にまとめています。

## できること

- 1回の依頼で最大1,000件の宛先にメールを並行送信(同時に送る数の上限は `SEND_CONCURRENCY`)
- 一時的な失敗の再送(間隔を空けながら)
- 重複防止キーによる二重配信の防止
- 送信結果を、署名付きの webhook でまとめて業務アプリに通知(届かなければ再送)
- 配信の件数の照会 API と、保持期間を過ぎたデータの削除

## これから

- 送信先サービスの上限に合わせた、秒あたりの流量制御
- サービスをまたいだトレース ID で、1通のメールを送信まで追跡(team-ops の T32)
- 目標性能(30万通を10分以内に送信完了)の負荷試験。結果は team-ops に公開します

## 設計方針

- 業務ロジック(誰に何を送るか)は持たない。文面は業務アプリ側で確定させる
- 宛先のメールアドレスは配信処理の間だけ保持し、配信記録には残さない
- 送信はSMTP。ローカル・デモ環境では Mailpit、実運用ではメール送信サービスに接続先を切り替える

## 設定(環境変数)

| 名前 | 必須 | 既定値 | 内容 |
|---|---|---|---|
| `DB_DSN` | ○ | | 専用スキーマへの接続先(例:`notify_relay:pass@tcp(mysql:3306)/notify_relay`) |
| `RELAY_SIGNING_KEYS` | ○ | | 署名の鍵。`鍵ID:base64の鍵` をカンマ区切りで複数指定でき、鍵の入れ替えに使う。鍵は32バイト以上 |
| `EMAIL_REF_KEY` | ○ | | ログと配信記録に、メールアドレスの代わりに残す値(email_ref)の鍵。base64で32バイト以上。鍵を使うので、ログを読める人が候補のアドレスと照合できない |
| `ALLOW_LOCAL_SIGNING_KEY` | | `false` | `true` のときだけ、team-ops に公開しているローカル開発用の鍵(署名の鍵と email_ref の鍵)を受け付ける |
| `SMTP_HOST` / `SMTP_PORT` | | `localhost` / `587` | 送信先のSMTPサーバー |
| `SMTP_TLS` | | `starttls` | `none`、`starttls`、`tls`。`none` は接続先が localhost か mailpit のときだけ使える |
| `SMTP_USERNAME` / `SMTP_PASSWORD` | | | SMTP認証(指定したときだけ使う) |
| `SMTP_FROM` / `SMTP_FROM_NAME` | | `no-reply@team-ops.invalid` / `チーム運営` | 差出人 |
| `SEND_CONCURRENCY` | | `50` | 同時に送る数 |
| `WEBHOOK_URL` | | | 送信結果の webhook の送り先(業務アプリ)。依頼ごとには指定させない。https が必須(localhost と compose の `api` だけ http を使える)。未設定のあいだ結果は送らずに残し、設定したあとで送る |
| `WEBHOOK_KEY_ID` | | 鍵が1つならその鍵 | webhook の署名に使う鍵ID(`RELAY_SIGNING_KEYS` のどれか) |
| `RECIPIENTS_INVALID_ONLY` | | `false` | `true` のとき `.invalid` ドメイン以外の宛先を拒否する(team-ops-api と同じ名前) |
| `DEMO_MODE` | | `false` | `RECIPIENTS_INVALID_ONLY` の別名。どちらかが `true` なら宛先を制限する |
| `PORT` / `SHUTDOWN_TIMEOUT` | | `8080` / `30s` | 待ち受けポートと、停止時に送信の完了を待つ時間 |

## 運用上の前提

- 1つのプロセスで動かす(二重送信を防ぐ仕組みの前提。docs/adr/0002)
- 依頼元(業務アプリ)は1つを前提とする。照会(GET /v1/deliveries/{id})は署名が正しければどの依頼も返し、送信結果の webhook はすべての依頼について WEBHOOK_URL の1か所に送る。複数のアプリで共有するときは、鍵IDごとに依頼と送り先を分ける作りが要る

## API仕様

[api/openapi.yaml](api/openapi.yaml)

## 利用例

少年スポーツチーム運営ツール [team-ops](https://github.com/yuukioda999/team-ops) のお知らせ配信で使用しています。

## ライセンス

[MIT](LICENSE)
