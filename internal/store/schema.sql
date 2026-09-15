-- atara-pay schema。非托管模型。
--
-- 这里**没有** wallets 表，这是刻意的：余额与托管仓位属于链，
-- 由 chain_* 表持有、只能隔着 chain.Chain 接口读。
-- 平台自己记一笔余额，就等于又变回托管了。
--
-- SQLite 方言：uuid/decimal/timestamp 一律 TEXT，enum 用 CHECK。
-- 金额存十进制字符串，运算全部在 Go 侧用 decimal 完成。

pragma foreign_keys = on;

-- 身份就是地址。邮箱只是通知渠道，不是登录名。
create table if not exists users (
  id            text primary key,
  address       text unique not null,
  display_name  text not null,
  email         text not null default '',
  kind          text not null default 'person' check (kind in ('person','firm','agent')),
  wallet_kind   text not null default 'atara' check (wallet_kind in ('atara','ext')),
  login_method  text not null default 'passkey',  -- passkey | wallet | google | email
  -- hue 为 0 表示前端按 id 哈希取色，与前端 PAV_HUES 的逻辑一致
  hue           integer not null default 0,
  avatar_url    text not null default '',
  -- reviewer 能审 maker 申请。审核不是 agent 共识，必须有真人入口。
  role          text not null default 'user' check (role in ('user','reviewer')),
  created_at    text not null
);

create table if not exists merchant_profiles (
  user_id             text primary key references users(id),
  peer_code           text unique not null,
  trust_score         integer not null,
  deals               integer not null default 0,
  disputes            integer not null default 0,
  fill_rate           text not null default '0',
  median_release_secs integer not null default 0,
  docs                text not null default '{}'
);

-- 联系人：一个字段收名字或地址，不再有 ATR ID。
create table if not exists contacts (
  owner_id    text not null references users(id),
  contact_id  text not null references users(id),
  label       text not null default '',   -- Supplier / Client / Colleague / Friend / My agent
  nickname    text not null default '',
  -- 加联系人是一次请求，不是一次收藏：对方得同意。pending 的联系人
  -- 不能被指定为收款方——否则「加了就能付」，对方从头到尾没说过话。
  status      text not null default 'accepted' check (status in ('pending','accepted')),
  created_at  text not null,
  primary key (owner_id, contact_id)
);

-- 额度。不是"卡"，是 allowance——签进账户合约，或对支出合约 approve。
create table if not exists allowances (
  id           text primary key,
  owner_id     text not null references users(id),
  spender      text not null,
  kind         text not null check (kind in ('person','agent')),
  asset        text not null default 'USDT',
  -- 额度是对某条链上某个代币合约的授权。不记链的话，同一个币在四条链上
  -- 的授权会混成一份，撤销时也不知道该去哪条链上撤。
  network      text not null default '',
  per_payment  text not null,
  window_cap   text not null,
  used         text not null default '0',
  cycle        text not null check (cycle in ('weekly','monthly')),
  expires_at   text,                       -- null = 不过期
  recipients   text not null default 'Any',
  template     text not null default '',
  wallet_kind  text not null default 'atara',
  chain_tx     text not null default '',
  status       text not null default 'live' check (status in ('live','revoked')),
  note         text not null default ''
);

create table if not exists offers (
  id            text primary key,
  maker_id      text not null references users(id),
  side          text not null check (side in ('buy','sell')),
  asset_code    text not null,
  network       text not null,
  networks      text not null,
  fiat_code     text not null,
  unit_price    text not null,
  qty           text not null,
  remaining_qty text not null,
  min_lot       text not null,
  lock_tx       text not null default '',   -- 挂出即锁币的链上凭证
  status        text not null default 'active' check (status in ('active','filled','delisted')),
  created_at    text not null,
  updated_at    text not null
);
create index if not exists idx_offers_browse on offers(status, side, asset_code, fiat_code);

-- R1 一笔一工单
create table if not exists orders (
  id              text primary key,
  ref             text unique not null,
  kind            text not null check (kind in ('conditional_transfer','otc_take')),
  owner_id        text not null references users(id),
  counterparty_id text references users(id),
  asset_code      text not null,
  amount          text not null,
  note            text not null default '',
  allowance_id    text references allowances(id),
  state           text not null,
  terminal        text check (terminal in ('completed','cancelled','expired','disputed')),
  state_deadline  text,
  funding_via     text not null default '',  -- wallet | external，谁出币谁选
  escrow_tx       text not null default '',
  escrow_addr     text not null default '',
  escrow_network  text not null default '',
  -- 下单那一刻算出来的风控评分，之后只读不重算。见 app/score.go：
  -- 重算的话历史单的分会跟着后来的事变，那就不是「当时的判断」了。
  trust_score     integer not null default 0,
  -- 手续费也是下单那一刻定的。存金额而不是只存费率：费率改了，
  -- 历史单上写的必须还是当时收的那个数。
  fee_amount      text not null default '0',
  fee_bps         integer not null default 0,
  -- 下单前跑的那次风控评估，整份存下来。
  --
  -- 不存的话它只在评估跑的那几秒里存在于前端内存里——刷新页面、换个设备、
  -- 事后复盘，全都看不到当初凭什么放行的。而这份「凭什么」正是这套东西
  -- 要给出的东西。存 JSON 不逐列建模：它是一份快照，不参与查询。
  assessment      text not null default '',
  created_at      text not null,
  updated_at      text not null
);
create index if not exists idx_orders_deadline on orders(state_deadline) where terminal is null;
create index if not exists idx_orders_owner on orders(owner_id, created_at desc);
create index if not exists idx_orders_peer on orders(counterparty_id, created_at desc);

create table if not exists order_conditional (
  order_id            text primary key references orders(id),
  main_branch         text not null,
  waiting_on          text not null,
  condition_text      text not null,
  fallback_days       integer not null default 14,
  dispute_window_secs integer not null default 0
);

create table if not exists order_conditions (
  order_id  text not null references orders(id),
  seq       integer not null check (seq between 1 and 3),
  atom_type text not null,
  params    text not null default '{}',
  primary key (order_id, seq)
);

create table if not exists order_otc (
  order_id    text primary key references orders(id),
  offer_id    text not null references offers(id),
  side        text not null,              -- taker 视角：buy | sell
  unit_price  text not null,
  fiat_code   text not null,
  fiat_amount text not null,
  network     text not null
);

create table if not exists order_events (
  id         integer primary key autoincrement,
  order_id   text not null references orders(id),
  seq        integer not null,
  from_state text,
  to_state   text not null,
  actor      text not null,
  reason     text not null default '',
  payload    text not null default '{}',
  created_at text not null,
  unique (order_id, seq)
);

-- 一个对手方一条线程：聊天、订单卡、系统播报、评估结论共用一条流。
create table if not exists messages (
  id         text primary key,
  owner_id   text not null references users(id),
  peer_id    text not null references users(id),
  author     text not null,               -- me | peer | system
  kind       text not null,               -- chat | system | order | assessment
  body       text not null default '',
  order_id   text,
  payload    text not null default '{}',
  created_at text not null
);
create index if not exists idx_messages_thread on messages(owner_id, peer_id, created_at);

-- 每条会话读到哪儿了。一行一条会话，不是一行一条消息：未读只需要一个
-- 分界线，逐条打标记的话，「全部标已读」要写几百行，而读到哪儿这件事
-- 本来就只有一个答案。
--
-- 未读只数对方说的话（author='them'）。系统播报不算——「Matched with X」
-- 常常是我自己下单触发的，给自己的动作挂一个未读角标，人会去点，然后
-- 发现没有任何新东西。
create table if not exists thread_reads (
  owner_id text not null references users(id),
  peer_id  text not null references users(id),
  read_at  text not null,
  primary key (owner_id, peer_id)
);

-- 链上动作的观察日志。不是账本——余额不在这里，这里只记"我们看到链上发生了什么"。
create table if not exists chain_events (
  id         integer primary key autoincrement,
  order_id   text,
  offer_id   text,
  actor_id   text,
  kind       text not null,               -- deposit | release | refund | listing_lock | listing_unlock | allowance
  asset      text not null default '',
  amount     text not null default '0',
  tx_hash    text not null default '',
  memo       text not null default '',
  created_at text not null
);

-- 法币腿只有凭证，没有余额
create table if not exists fiat_receipts (
  id          text primary key,
  order_id    text not null references orders(id),
  uploader_id text not null references users(id),
  file_ref    text not null,
  verified_at text,
  created_at  text not null
);

create table if not exists uploads (
  id           text primary key,
  owner_id     text not null,
  file_ref     text unique not null,
  filename     text not null,
  content_type text not null,
  size_bytes   integer not null,
  created_at   text not null
);

-- 收款方：非托管下链上转账由用户自己签，平台只记地址簿。
create table if not exists payees (
  id         text primary key,
  owner_id   text not null references users(id),
  label      text not null,
  chain      text not null,
  address    text not null,
  created_at text not null,
  unique (owner_id, chain, address)
);

-- 提现：记的是意图与合规材料，不代持资金。tx_hash 由用户签完回填。
-- 法币收款账户：只存自己的。
--
-- OTC 的法币腿点对点走银行——账号是给对手方的，钱不经过平台。对手方的
-- 银行信息属于那笔交易，不属于我的账户簿，所以不混在这张表里。
--
-- account_no 存的是**掩码后的号**（首四末四），不是全量。全量只在提交那一刻
-- 用来校验，校完就丢。这样即使库被拖走，里面也没有可以直接拿去用的账号；
-- 代价是改号必须重打一遍——那正是它该有的代价。
create table if not exists bank_accounts (
  id          text primary key,
  owner_id    text not null references users(id),
  holder      text not null,
  bank        text not null,
  account_no  text not null,
  currency    text not null,
  region      text not null,
  created_at  text not null,
  updated_at  text not null
);
create index if not exists idx_bank_owner on bank_accounts(owner_id);

create table if not exists withdrawals (
  id            text primary key,
  owner_id      text not null references users(id),
  -- 可空：非托管的转账本来就允许打给任意地址，不强制先登记。
  -- 必须是 NULL 而不是空串——SQLite 的外键只豁免 NULL，空串照样去 payees
  -- 里找一行 id='' 然后失败。登记过的填 payee_id，没登记的把地址记在
  -- to_address / to_chain 上。
  payee_id      text references payees(id),
  to_address    text not null default '',
  to_chain      text not null default '',
  asset_code    text not null,
  amount        text not null,
  -- 用途不再强制：那是托管所的提币流程要的。钱不在我们手里，
  -- 我们既拦不住这笔转账，也没有立场问「为什么转」。
  purpose       text not null default '',
  doc_upload_id text not null default '',
  tx_hash       text not null default '',
  state         text not null default 'draft'
                check (state in ('draft','submitted','broadcast','confirmed','failed')),
  created_at    text not null,
  updated_at    text not null
);

-- Maker 申请。九步 KYC 字段太碎且前端仍在改，整体存 JSON blob。
create table if not exists maker_applications (
  user_id       text primary key references users(id),
  phase         text not null default 'kyc' check (phase in ('kyc','listing')),
  kyc_done      integer not null default 0,
  kyc_ok        integer not null default 0,
  listing_done  integer not null default 0,
  approved      integer not null default 0,
  form_json     text not null default '{}',
  reject_reason text not null default '',
  submitted_at  text,
  reviewed_at   text,
  reviewer_id   text references users(id),
  -- 演示用：到点自动放行。存时间戳而不是起一个睡 5 秒的 goroutine——
  -- 进程重启后 goroutine 就没了，申请会永远卡在「审核中」。
  auto_review_at text,
  -- 模型层没跑成时的重试时间。
  --
  -- 模型读不了不是「你材料有问题」，也不该为此惊动人——绝大多数是一次
  -- 抖动，隔一会儿再问一次就好。存时间戳同上：起 goroutine 的话进程一重启
  -- 就没人再管这份申请了。
  ai_retry_at    text,
  -- 试过几次。连着失败到上限才转人工——那时它已经不是抖动，是真出事了，
  -- 而真出事就该有人知道。
  ai_attempts    integer not null default 0,
  -- 申请人的申诉。非空表示他不认预审的结论，这一份在等人看。
  --
  -- 为什么要留这条路：预审判错了而没有任何路径能推翻它，这个商户就被
  -- 永久锁在门外——他改也没用，因为他本来就没错。可以一个月零次，
  -- 但不能不存在。
  appeal_note    text not null default '',
  appealed_at    text,
  updated_at    text not null
);

-- 准入预审。一次审一行。
--
-- 为什么不在 maker_applications 上加列覆盖式存：留痕要能回答「从哪天开始
-- 判得不对的」。覆盖式存不了历史，模型换了版本、或者哪天开始误判，都没有
-- 可比对的东西。跟下面 kyc_checks 是同一个取舍。
create table if not exists maker_reviews (
  id          text primary key,
  user_id     text not null references users(id),
  stage       text not null check (stage in ('kyc','listing')),
  -- 谁出的这一票：规则层 / 模型 / 人
  source      text not null check (source in ('rule','ai','human')),
  verdict     text not null check (verdict in ('pass','revise','escalate')),
  -- 逐项问题的原文。摘要另存 maker_applications.reject_reason，
  -- 那一列是给已有的 admin 后台和 AI 聊天窗读的。
  issues_json text not null default '[]',
  -- 模型版本。换版本要能把前后切开，不然「是不是换版本之后开始判错的」
  -- 这个问题没有答案。规则层留空。
  model_id    text not null default '',
  -- 送审那份表单的哈希。同一份材料换个模型判得一样吗——要复盘就得有它。
  input_hash  text not null default '',
  latency_ms  integer not null default 0,
  created_at  text not null
);
create index if not exists idx_maker_reviews_user on maker_reviews(user_id, created_at desc);

-- 身份核验。一次 DocuPass 会话一行，以 reference 为主键。
--
-- 为什么不是一个用户一行：一个人可能核好几次（第一次证件糊了、链接过期、
-- 被判 review 后重新走一遍）。只留最后一次的话，"他什么时候通过的、当时
-- 那次为什么被拒" 这类问题就没有答案了，而这正是合规上要留痕的东西。
--
-- raw_json 是 ID Analyzer 回来的整份原文。只存我们解析出的字段是不够的：
-- 出纠纷时要查的是它当时到底说了什么，不是我们当时读懂了什么。
create table if not exists kyc_verifications (
  reference      text primary key,
  user_id        text not null references users(id),
  -- pending：会话建好了，人还没走完 / 结果还没回来
  status         text not null default 'pending'
                 check (status in ('pending','accept','review','reject')),
  transaction_id text not null default '',
  docupass_id    text not null default '',
  profile_id     text not null default '',
  review_score   integer not null default 0,
  reject_score   integer not null default 0,
  -- 最后一次落库的事件名。和 transaction_id 一起用来去重：
  -- 回调会重投（失败重试 4 次，门户里还能手动重发 48 小时）。
  last_event     text not null default '',
  identity_json  text not null default '{}',
  warnings_json  text not null default '[]',
  raw_json       text not null default '',
  -- 结论是从哪条路来的：webhook 还是我们自己拉的。查"为什么没更新"时，
  -- 先要分清是回调没到，还是拉取没跑。
  source         text not null default '',
  created_at     text not null,
  updated_at     text not null,
  concluded_at   text
);
create index if not exists idx_kyc_user on kyc_verifications(user_id, created_at desc);
create index if not exists idx_kyc_txn on kyc_verifications(transaction_id);

-- 支付确认令牌。原先是进程内的 map，重启即丢；落库后重启不影响未过期的令牌。
create table if not exists confirmations (
  token       text primary key,
  user_id     text not null references users(id),
  digest      text not null,
  grade       text not null,
  expires_at  text not null,
  consumed_at text
);
create index if not exists idx_confirmations_expiry on confirmations(expires_at);

-- 合约部署记录。前端要自己发交易，就得知道 approve 给谁、锁进哪个合约、
-- 代币合约是哪个——这些从这里发下去，不是写死在前端，也不是每次从 env 现读。
--
-- 一条链一行。留着历史行是有意的：换过一次合约之后，旧地址还能查到，
-- 排查「这笔钱当时打进了哪个合约」时那一行就是答案。
create table if not exists chain_deployments (
  network       text primary key,
  chain_id      integer not null default 0,
  impl          text not null default 'evm',
  rpc_url       text not null default '',
  explorer      text not null default '',
  escrow_addr   text not null default '',
  spending_addr text not null default '',
  updated_at    text not null
);

-- 代币合约。精度必须存下来：BSC 上稳定币 18 位、以太坊上 6 位，
-- 前端拿它算金额，猜错差 10^12。
create table if not exists chain_tokens (
  network  text not null references chain_deployments(network),
  asset    text not null,
  address  text not null,
  decimals integer not null default 0,
  primary key (network, asset)
);

-- 后台操作审计。append-only：每次 admin 写动作记一行，事后可查「谁在何时干了什么」。
-- 不改任何业务表，只是旁路留痕。actor_id 是执行动作的账户（真鉴权前是 reviewer）。
create table if not exists admin_audit (
  id          integer primary key autoincrement,
  actor_id    text not null,
  action      text not null,            -- maker.review | offer.delist | withdrawal.review | user.ban | user.unban | user.revoke_maker
  target_type text not null default '', -- application | offer | withdrawal | user
  target_id   text not null default '',
  detail      text not null default '', -- JSON 或一句话，随动作而定
  created_at  text not null
);
create index if not exists idx_admin_audit_time on admin_audit(created_at desc);

-- AI 助手（Atara AI / DeepSeek）调用日志。对话内容本身存在 messages 表里
-- （peer_id = 'user-desk'）；这张表记的是每次调用的运维指标：谁、什么模型、
-- 多久、成没成、进出多少字。token/成本要改流式接口拿 usage，先不做。
create table if not exists ai_calls (
  id           integer primary key autoincrement,
  user_id      text not null,
  model        text not null default '',
  ok           integer not null default 1,
  err          text not null default '',
  input_chars  integer not null default 0,
  output_chars integer not null default 0,
  latency_ms   integer not null default 0,
  -- token 用量与估算成本。prompt/completion 来自上游 usage；cost_micros 是
  -- 按模型单价估算的微美元（cost_usd = cost_micros/1e6），单价见 app/desk.go。
  prompt_tokens     integer not null default 0,
  completion_tokens integer not null default 0,
  total_tokens      integer not null default 0,
  cost_micros       integer not null default 0,
  created_at   text not null
);
create index if not exists idx_ai_calls_time on ai_calls(created_at desc);

-- 后台可调的键值设置（目前只有 AI 人设 desk_persona）。留一张通用表，
-- 后面别的可调项也往这儿放，不用一项建一张表。
create table if not exists app_settings (
  key        text primary key,
  value      text not null default '',
  updated_by text not null default '',
  updated_at text not null
);

-- 管理员账号。独立于消费端 users（那边是钱包/社交自助注册）——后台是内部员工，
-- 指派制、账号密码登录。role 预留 reviewer/admin 之分，当前都当管理员用。
create table if not exists admin_accounts (
  id            text primary key,
  email         text unique not null,
  password_hash text not null,
  name          text not null default '',
  role          text not null default 'admin' check (role in ('reviewer','admin')),
  disabled      integer not null default 0,
  created_at    text not null
);

-- 管理员会话 token。不透明随机串，可吊销、会过期——和 confirmations 一个套路。
create table if not exists admin_sessions (
  token      text primary key,
  admin_id   text not null references admin_accounts(id),
  expires_at text not null,
  created_at text not null
);
create index if not exists idx_admin_sessions_expiry on admin_sessions(expires_at);
