-- 模拟链自己的账本。这几张表属于链，不属于 Atara——
-- 后端只能通过 chain.Chain 接口读写，任何直接 SQL 都是越界。

create table if not exists chain_balances (
  address    text not null,
  asset      text not null,
  amount     text not null default '0',
  primary key (address, asset)
);

create table if not exists chain_deposits (
  order_id    text primary key,
  asset       text not null,
  amount      text not null,
  from_addr   text not null default '',
  via         text not null,              -- wallet | external
  tx_hash     text not null default '',
  started_at  text not null,              -- 用于按墙钟时间推算确认数
  detected_at text,
  required    integer not null
);

create table if not exists chain_positions (
  order_id  text primary key,
  offer_id  text not null default '',
  owner     text not null,
  asset     text not null,
  amount    text not null,
  contract  text not null,
  network   text not null,
  tx_hash   text not null default '',
  status    text not null                 -- pending | escrowed | released | refunded
);

create table if not exists chain_listing_locks (
  offer_id text primary key,
  owner    text not null,
  asset    text not null,
  amount   text not null,
  tx_hash  text not null,
  status   text not null                  -- locked | unlocked
);

create table if not exists chain_allowances (
  id          text primary key,
  account     text not null,
  wallet_kind text not null,
  spender     text not null,
  asset       text not null,
  per_payment text not null,
  window_cap  text not null,
  cycle       text not null,
  expires_at  text,
  tx_hash     text not null,
  status      text not null               -- live | revoked
);
