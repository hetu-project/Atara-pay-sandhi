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
create table if not exists withdrawals (
  id            text primary key,
  owner_id      text not null references users(id),
  payee_id      text not null references payees(id),
  asset_code    text not null,
  amount        text not null,
  purpose       text not null,
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
  updated_at    text not null
);

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
