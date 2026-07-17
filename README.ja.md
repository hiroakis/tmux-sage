# tmux-sage

[English](README.md) | 日本語

tmux の各ウィンドウ（タブ）で行われている作業を LLM で要約し、その要約をウィンドウ名として自動設定するデーモンです。

複数ペインを開いているウィンドウでも、**すべての**ペインの画面内容・実行中コマンド・作業ディレクトリをまとめて要約するため、タブ名を見るだけでそのタブが何のためのものか分かります。

## 仕組み

1. 一定間隔で全ウィンドウを巡回し、各ペインの画面下部 N 行（`capture-pane`）とペインのメタデータを収集します。
2. 前回の要約から内容のハッシュが変わっていなければスキップします（API 呼び出しなし）。
3. 変わっていれば Anthropic API（デフォルト Claude Haiku 4.5）で短いラベルと長めの説明を生成します。
4. ラベルは `tmux rename-window` で反映し（前回と同じならスキップ）、説明はウィンドウのユーザーオプション `@sage_desc` に保存します。
5. 状態（内容ハッシュ `@sage_hash`、最終呼び出し時刻 `@sage_last_call`）も tmux のウィンドウオプションに永続化します。tmux-sage を再起動しても、内容が変わっていない・直近に要約済みのウィンドウは再要約されません。tmux サーバを再起動すると状態はリセットされます。

## 必要なもの

- tmux
- Anthropic API キー（環境変数 `ANTHROPIC_API_KEY`）

## インストール

### Homebrew（macOS）

```sh
brew install --cask hiroakis/tap/tmux-sage
```

`tmux-sage` バイナリが `PATH` に入るので、下記の TPM プラグインが自動でバイナリを見つけられるようになります。

### TPM を使う（推奨）

`~/.tmux.conf` に追加:

```tmux
set -g @plugin 'hiroakis/tmux-sage'

# 任意の設定（デフォルト値を表示）
set -g @sage_mode 'daemon'    # daemon | hook | off
set -g @sage_args ''          # 追加フラグ。例: '-lang Japanese -min-api-interval 300s'
```

`prefix + I` でインストールします。Go がインストールされていればプラグインが `go build` でバイナリをビルドします（ない場合は `@sage_bin` にビルド済みバイナリのパスを指定するか、`PATH` に `tmux-sage` を置いてください）。

`ANTHROPIC_API_KEY` が tmux サーバから見える必要があります。キーを export したシェルから tmux を起動するか、明示的に設定してください:

```sh
tmux set-environment -g ANTHROPIC_API_KEY sk-ant-...
```

| プラグインオプション | デフォルト | 説明 |
|---|---|---|
| `@sage_mode` | `daemon` | `daemon` はバックグラウンドで常駐、`hook` はウィンドウ切替時のみ要約、`off` は無効 |
| `@sage_args` | （空） | tmux-sage に渡す追加のコマンドラインフラグ |
| `@sage_bin` | 自動検出 | tmux-sage バイナリのパス（プラグインディレクトリ → `PATH` → `go build` の順で解決） |
| `@sage_log` | `$TMPDIR/tmux-sage.log` | デーモン / フックのログファイル |

### 手動

```sh
go build -o tmux-sage .
export ANTHROPIC_API_KEY=sk-ant-...

# まず dry-run で試す（リネームせずラベルを表示）
./tmux-sage -once -dry-run

# デーモンとして常駐
./tmux-sage &
```

## デーモンモード vs フックモード

**デーモンモード（デフォルト・推奨）。** バックグラウンドプロセスが一定間隔で全ウィンドウを巡回するため、**見ていない**ウィンドウの名前も最新に保たれます。バックグラウンドのタブでビルドが終わった、長時間実行のエージェントが完了した、といった変化がタブ名に反映されます。アイドル状態のウィンドウにコストはかかりません。内容が変わっていなければ API 呼び出しの前にハッシュ比較でスキップされます。

**フックモード。** 常駐プロセスなしで、カレントウィンドウが変わるたびに1回だけ実行されます。ウィンドウごとの状態（内容ハッシュ・最終呼び出し時刻）が tmux のウィンドウオプションに永続化されているため、実行をまたいでもデバウンスが正しく効きます。トレードオフとして、バックグラウンドのウィンドウ名は tmux を操作したときにしか更新されません。TPM（`set -g @sage_mode 'hook'`）または手動で有効化できます:

```tmux
set-hook -g session-window-changed "run-shell -b 'tmux-sage -once >>/tmp/tmux-sage.log 2>&1'"
```

> **注意:** TPM のインストーラ（`prefix + I`）はプラグインをダウンロードするだけです。フックはプラグインスクリプトの実行時に登録されるため、インストール後に `tmux source-file ~/.tmux.conf` で設定を再読み込みしてください。

cron などの任意のトリガーから `tmux-sage -once` を実行しても構いません。状態が永続化されているため、どの呼び出しも安全かつ低コストです。

## オプション

| フラグ | デフォルト | 説明 |
|---|---|---|
| `-interval` | `30s` | 巡回間隔 |
| `-min-api-interval` | `180s` | ウィンドウごとの要約の最小間隔 |
| `-lines` | `30` | 各ペインの画面下部からキャプチャする行数 |
| `-max-label-len` | `20` | ラベルの最大文字数 |
| `-max-desc-len` | `60` | 説明の最大文字数（`@sage_desc` に保存） |
| `-model` | `claude-haiku-4-5` | Anthropic のモデル ID |
| `-lang` | `English` | 生成するラベル・説明の言語（例: `English`、`Japanese`、`ja`、`fr`） |
| `-redact` | `true` | LLM に送信する前に、ペイン内容中の秘匿情報らしき文字列（API キー、トークン、`Authorization:` ヘッダ）をマスク |
| `-change-threshold` | `0.1` | 再要約に必要な変化行の割合（0 = どんな変化でも対象）。TUI アプリのスピナーや時計だけの変化を除外 |
| `-max-cost-per-day` | `0` | 1日（暦日）の消費額がこの USD 額に達したら API 呼び出しを停止（0 = 無制限）。ローカル時刻の日付変更でリセット。プロセス内メモリで管理 |
| `-dry-run` | `false` | リネームせずラベルを表示 |
| `-once` | `false` | 1回だけ実行して終了 |
| `-verbose` | `false` | スキップ判定（変化なし / デバウンス中）もウィンドウごとにログ出力 |

## choose-window に長い説明を表示する

tmux-sage は各ウィンドウの作業内容の長めの説明を `@sage_desc` ウィンドウオプションに保存します。choose-window（`choose-tree -w`）のフォーマットをカスタマイズすると、ウィンドウ一覧に説明を表示できます。`~/.tmux.conf` に:

```tmux
bind Space choose-tree -w -F '#{window_name}#{window_flags} #{?#{!=:#{@sage_desc},},— #{@sage_desc},}'
```

デフォルトのフォーマットで表示されていたアクティブペインのタイトルも残したい場合は、フォーマット文字列に `"#{pane_title}"` を追加してください。

## 特定のウィンドウを対象外にする

対象外にしたいウィンドウで:

```sh
tmux set-option -w @sage_off 1
```

解除は `tmux set-option -wu @sage_off` です。

## Roadmap / TODO

プロバイダ対応:

- [ ] **Anthropic 以外のプロバイダ対応** — summarizer をインターフェースで抽象化し、OpenAI 互換バックエンド（`-provider`、`-base-url` フラグ）を追加する。OpenAI 互換エンドポイントは OpenAI 本体に加えて **ローカル LLM**（Ollama、llama.cpp、LM Studio、vLLM）もカバーでき、ペイン内容を外部 API に送ることへのプライバシー懸念の解決にもなる
- [ ] Google Gemini バックエンド
- [ ] AWS Bedrock / Google Vertex AI バックエンド（クラウド内に閉じる必要があるチーム向け）

機能:

- [x] LLM に送信する前にペイン内容から明らかな秘匿情報（API キー、トークン、`Authorization:` ヘッダ）をマスク（`-redact`）
- [x] 変化検知の改善 — スピナーや時計表示だけの変化を無視（`-change-threshold`）
- [x] 1日のコスト上限（`-max-cost-per-day`）— 予算超過で API 呼び出しを停止
- [ ] 日次消費額の再起動をまたいだ永続化（現状はプロセス内メモリ管理）
- [ ] プロンプトテンプレートのカスタマイズ（`-prompt-file`）
- [ ] ウィンドウ名だけでなくセッション名の要約
- [ ] フラグの代替としての設定ファイル（`~/.config/tmux-sage/config.toml`）

配布:

- [x] LICENSE ファイル（MIT）
- [x] GitHub Actions CI（build、vet、test）
- [x] goreleaser によるビルド済みバイナリ配布、Homebrew tap（`hiroakis/tap/tmux-sage`）

## 注意

- ペインの画面内容は要約のために Anthropic API へ送信されます。秘匿情報らしき文字列はデフォルトでマスクされますが（`-redact`）、パターンマッチによるベストエフォートです。機密情報が表示されうるペインを持つウィンドウには `@sage_off` を設定してください。
- 自動リネームされたウィンドウは tmux の `automatic-rename` がオフになります（手動リネームと同じ挙動です）。
- LLM 呼び出しが成功するたびに、トークン使用量とコスト、累計をログに出力します。モデルごとの単価は `pricePerMTok()` にハードコードされています。価格が変わった場合は更新してください。
