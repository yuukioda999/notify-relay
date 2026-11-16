# ADR-0003: 受け付けと取り出しのトランザクションを READ COMMITTED にする

- ステータス: 承認
- 日付: 2026-11-16
- 決定者: 小田 裕貴
- 関連: ADR-0002(二重送信が起こりうる時間)、AC-R001-01・08、team-ops の障害訓練(spec 009)の振り返り「notify-relay の停止」
- 番号について:このリポジトリのコードのコメントにある「ADR-0003」の多くは、team-ops の ADR-0003(個人情報を最小にする)を指す。この ADR を指すときは「notify-relay の ADR-0003」と書く

## コンテキスト

- team-ops の障害訓練で、依頼の受け付け(依頼と宛先の行の insert)が MySQL の deadlock(Error 1213)で失敗することが、何度もあった(エラーを数え始めてからの10の run のうち5つ)。受け付けはやり直さずに 503 を返すので、業務アプリは自分の送り直しの間隔(失敗の回数に応じて30秒〜30分。team-ops-api の DeliverOutboxMessage::BACKOFF)を待つ。1件が約6分遅れた回があり、この失敗が原因と見ている
- 送信の取り出し(`SELECT ... FOR UPDATE SKIP LOCKED` で宛先を選び、`UPDATE ... SET status = 'sending'`)では、すでに deadlock のときに3回まで実行している(やり直しは2回まで)
- 訓練の環境(MySQL 8.4)で取った deadlock の記録(2026-11-16、team-ops の run 43861779973。アドレスは伏せた。行の中身は省いた):

```
*** (1) TRANSACTION:
INSERT INTO delivery_recipients (delivery_id, recipient_ref, email, email_ref, status, next_attempt_at, updated_at) VALUES (...)
*** (1) HOLDS THE LOCK(S):
index PRIMARY of table `notify_relay`.`delivery_recipients` lock_mode X locks rec but not gap
*** (1) WAITING FOR THIS LOCK TO BE GRANTED:
index delivery_recipients_due_index of table `notify_relay`.`delivery_recipients` lock_mode X locks gap before rec insert intention waiting
*** (2) TRANSACTION:
UPDATE delivery_recipients SET status = 'sending', updated_at = ... WHERE id IN (1,2,3,...)
*** (2) HOLDS THE LOCK(S):
index delivery_recipients_due_index of table `notify_relay`.`delivery_recipients` lock_mode X locks gap before rec
```

- 読み方:受け付けの insert は、宛先の索引(`delivery_recipients_due_index`、状態と送る時刻)のすき間に入るためのロック(insert intention)を待っている。そのすき間のロック(gap lock)を、取り出しのトランザクション(UPDATE を実行中)が持っている。そのロックを、先の `SELECT ... FOR UPDATE` で取ったのか UPDATE で取ったのかは、記録からは分からない。MySQL の既定の分離レベル(REPEATABLE READ)では、ロックを取る読み取りと更新が、行に加えて索引のすき間もロックするためと見ている。記録は `(2) HOLDS THE LOCK(S)` で終わっていて、取り出しの側が待っていたロックと、どちらをロールバックしたかは載っていない(切れた理由は確かめていない)
- 手元の MariaDB では、取り出しと受け付けを同時に走らせるテストで同じ形の deadlock が再現した。CI の MySQL 8.4 では、修正前のコードでも同じテストで再現しなかった(10回)。テストの取り出しの数を本番と同じ500にしたが、MySQL 8.4 で修正前に落ちるかは確かめていない。このテストが再発を防げるのは、確かめた範囲では MariaDB だけ

## 検討した選択肢

| 案 | 内容 | 評価 |
|---|---|---|
| A | 受け付けでも、取り出しと同じく deadlock のときに3回まで実行する | 小さな変更。ただし、手元の MariaDB で取り出しと受け付けを同時に走らせるテストでは、やり直しだけでは失敗が残った(30回中2回) |
| **B(採用)** | A に加えて、受け付けと取り出しのトランザクションを READ COMMITTED にする | 取り出しの読み取りと更新が、すき間のロックを取らなくなる。同じテストで失敗0(手元の MariaDB で30回と100回) |
| C | やり直しの回数を増やす | 混んでいるほど待ちが延び、原因が残る |

## 決定

- 受け付け(`Create`)と取り出し(`claim`)のトランザクションを READ COMMITTED で始める
- 受け付けも deadlock のときは3回まで実行する(やり直しは2回まで)。ロック待ちの時間切れ(既定50秒)は、HTTP の応答の時間(30秒)より長いので、やり直さない
- 結果の記録(`record`)と webhook の処理は変えない。どちらも deadlock をやり直していて、訓練で出た deadlock(記録どうし、webhook のイベントどうし)は、やり直しで済んでいる

## 結果

良くなること:

- 業務アプリから依頼が一度に届いても、受け付けが deadlock で失敗しにくくなる見込み。MySQL 8.4 での効果は、修正後の障害訓練の run(受け付けの失敗のログが出ないこと)で確かめる

悪くなること・受け入れるコスト:

- READ COMMITTED でも、重複キーの確認などでは、すき間のロックを取りうる(受け付けは2つの一意索引を確かめる)。deadlock が0になるわけではないので、やり直しを残す
- READ COMMITTED では、同じトランザクションの中で同じ行を2回読むと、ほかのトランザクションが確定した変更が見える。受け付けは insert だけ、取り出しは「ロックを取って読み、その行を更新する」だけで、2回読んで比べる処理はない。同じ宛先を2つの取り出しに渡さないことは、行のロック(`FOR UPDATE SKIP LOCKED`)で守り、受け付けと同時に動かすテスト(AC-R001-08)で確かめている
- 分離レベルの違いを知らずに、取り出しのトランザクションに読み直しの処理を足すと、思わぬ結果になりうる。`claim` のコメントにこの ADR を書いた
- 前提:READ COMMITTED では、MySQL のバイナリログが行の形式(ROW)である必要がある。MySQL 8.4 の既定は ROW で、team-ops の compose は変えていない

## 見直す条件

- 受け付けや取り出しに、同じ行を読み直して判断する処理を足すとき
- READ COMMITTED でも、受け付けの deadlock が続くとき(ログの `accept delivery` で分かる)
