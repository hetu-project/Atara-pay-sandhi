-- atara-pay 一期 schema。
-- SQLite 方言：uuid/decimal/timestamp 一律 TEXT，enum 用 CHECK 约束。
-- 金额存十进制字符串，比较与运算全部在 Go 侧用 decimal 完成——
-- 不让 SQLite 的浮点参与任何资金判断。

pragma foreign_keys = on;

create table if not exists users (
  id           text primary key,
  handle       text unique not null,
  email        text unique not null,
  display_name text not null,
  kind         text not null default 'person' check (kind in ('person','firm','agent')),
  created_at   text not null
);

-- 挂单卡上必须出现的字段：缺件也公开，让买家自己给缺口定价
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

-- 授权卡：人自己也是一张卡，agent 各是一张
create table if not exists authorization_cards (
  id           text primary key,
  owner_id     text not null references users(id),
  name         text not null,
  kind         text not null check (kind in ('person','agent')),
  cycle        text not null check (cycle in ('weekly','monthly')),
  quota        text,
  used         text not null default '0',
  per_deal_cap text,
  allowlist    text not null default '',
  template     text not null default '',
  enabled      integer not null default 1,
  note         text not null default ''
);

-- 钱包只持有数字资产。法币不入账：法币腿点对点走银行，平台只核验回执。
create table if not exists wallets (
  id         text primary key,
  user_id    text not null references users(id),
  asset_code text not null,
  available  text not null default '0',
  escrowed   text not null default '0',
  unique (user_id, asset_code)
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
  card_id         text references authorization_cards(id),
  state           text not null,
  terminal        text check (terminal in ('completed','cancelled','expired','disputed')),
  state_deadline  text,
  created_at      text not null,
  updated_at      text not null
);
create index if not exists idx_orders_deadline on orders(state_deadline) where terminal is null;
create index if not exists idx_orders_owner on orders(owner_id, created_at desc);

create table if not exists order_conditional (
  order_id            text primary key references orders(id),
  main_branch         text not null,
  waiting_on          text not null,
  condition_text      text not null,
  fallback_days       integer not null default 14,
  dispute_window_secs integer not null default 0
);

-- 最多 3 个原子的 AND 组合；空集 = 立即释放
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
  side        text not null,
  unit_price  text not null,
  fiat_code   text not null,
  fiat_amount text not null,
  network     text not null
);

-- 状态变化留痕：争议时这条线程就是证据链的一部分
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

create table if not exists ledger_entries (
  id              integer primary key autoincrement,
  wallet_id       text not null references wallets(id),
  order_id        text,
  offer_id        text,
  kind            text not null,
  delta_available text not null,
  delta_escrowed  text not null,
  memo            text not null default '',
  created_at      text not null
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
