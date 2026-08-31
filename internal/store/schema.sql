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
