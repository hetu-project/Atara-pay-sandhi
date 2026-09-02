// SPDX-License-Identifier: MIT
pragma solidity 0.8.24;

/// @notice ERC-20 的最小接口。TRC-20 与之兼容，所以同一份合约能部到 TRON。
interface IERC20 {
    function transfer(address to, uint256 amount) external returns (bool);
    function transferFrom(address from, address to, uint256 amount) external returns (bool);
    function balanceOf(address account) external view returns (uint256);
}

/**
 * @title AtaraEscrow
 * @notice Atara 结算协议的托管合约。
 *
 * 设计的要害只有一条：**资金只能通过带阈值签名证明的调用离开这个合约**。
 *
 * 没有 owner 提款、没有「运营方放行」、没有紧急转出。如果后端能直接调
 * release()，那「放行由共识决定」这件事就只存在于后端，合约只是个傀儡——
 * 协议声称的「放行由可核验的凭证触发，不由平台的话」在链上就是假的。
 * 所以放行与退款都要求 N-of-M 的共识签名方对一份 EIP-712 证明签名，
 * 合约自己验签、自己数人数、自己比分数。
 *
 * owner 能做的事被刻意限制在治理层：换签名方名单、改阈值、暂停新入金。
 * owner **不能**动任何一笔已托管的资金，也不能让暂停影响已存在的仓位放行——
 * 否则暂停就成了变相的冻结。
 */
contract AtaraEscrow {
    // ── 类型 ──

    enum Status {
        None, // 从未存在
        Escrowed, // 资金在合约里，等放行条件
        Released, // 已放给收款方
        Refunded, // 已原路退回
        Disputed // 有异议，资金保持锁定，需裁决证明才能动
    }

    /// @notice 一笔工单在合约里的仓位。
    struct Position {
        address token;
        uint256 amount;
        /// @dev 出币的一方。退款回到这里。
        address payer;
        /// @dev 收款方。放行到这里。
        address beneficiary;
        /// @dev 非零表示这批币来自挂单锁仓，退款要还回挂单而不是个人余额。
        bytes32 offerId;
        Status status;
    }

    /// @notice 做市方挂单时锁进来的仓位。挂出即锁币，买家看到的可成交量是真的。
    struct ListingLock {
        address maker;
        address token;
        uint256 total;
        /// @dev 已被订单绑走的量。available = total - bound。
        uint256 bound;
        bool open;
    }

    enum Verdict {
        Release, // 共识通过：放给收款方
        Refund, // 共识不通过或条件未成立：原路退回
        Hold // 拦下转人工：不动资金，转入 Disputed
    }

    /**
     * @notice 共识出具的放行证明。
     * @param orderId  这份证明只对这一笔工单有效。
     * @param verdict  共识的结论。
     * @param score    共识评分（0-100）。放行要求 score >= minScore。
     * @param nonce    防重放。同一份证明只能用一次。
     * @param deadline 证明的有效截止时间。过期的共识结论不该还能放款。
     */
    struct Attestation {
        bytes32 orderId;
        Verdict verdict;
        uint16 score;
        uint256 nonce;
        uint256 deadline;
    }

    // ── 存储 ──

    address public owner;

    /// @dev 共识签名方名单。放行需要其中 threshold 个不同地址签名。
    mapping(address => bool) public isSigner;
    address[] private signerList;

    /// @notice 放行所需的最少签名方数量。
    uint256 public threshold;

    /// @notice 放行所需的最低共识评分。
    uint16 public minScore;

    /// @notice 暂停后不再接受新入金。**不影响已存在仓位的放行与退款**——
    /// 否则暂停就等于冻结用户资金，那是托管，不是非托管。
    bool public depositsPaused;

    mapping(bytes32 => Position) private positions;
    mapping(bytes32 => ListingLock) private listings;

    /// @dev 用过的证明摘要。防重放的执行点。
    mapping(bytes32 => bool) public attestationUsed;

    // ── EIP-712 ──

    bytes32 private constant ATTESTATION_TYPEHASH = keccak256(
        "Attestation(bytes32 orderId,uint8 verdict,uint16 score,uint256 nonce,uint256 deadline)"
    );
    bytes32 private constant DOMAIN_TYPEHASH =
        keccak256("EIP712Domain(string name,string version,uint256 chainId,address verifyingContract)");

    /// @dev 缓存域分隔符，但记下当时的 chainId——分叉后必须重算，
    /// 否则一条链上的证明能在另一条链上重放。
    bytes32 private cachedDomainSeparator;
    uint256 private cachedChainId;

    // ── 事件 ──

    event Deposited(bytes32 indexed orderId, address indexed payer, address token, uint256 amount);
    event ListingLocked(bytes32 indexed offerId, address indexed maker, address token, uint256 amount);
    event ListingUnlocked(bytes32 indexed offerId, uint256 returned);
    event ListingBound(bytes32 indexed offerId, bytes32 indexed orderId, uint256 amount);
    event Released(bytes32 indexed orderId, address indexed to, uint256 amount, uint16 score);
    event Refunded(bytes32 indexed orderId, address indexed to, uint256 amount);
    event Disputed(bytes32 indexed orderId, address indexed by);
    event SignersChanged(address[] signers, uint256 threshold, uint16 minScore);
    event DepositsPaused(bool paused);
    event OwnerChanged(address indexed from, address indexed to);

    // ── 错误 ──

    error NotOwner();
    error NotParty();
    error DepositsArePaused();
    error ZeroAmount();
    error ZeroAddress();
    error OrderExists();
    error NoPosition();
    error NotEscrowed();
    error NotDisputed();
    error ListingNotOpen();
    error ListingInsufficient();
    error AttestationExpired();
    error AttestationReplayed();
    error WrongVerdict();
    error ScoreTooLow();
    error NotEnoughSigners();
    error BadSignature();
    error SignersUnsorted();
    error TransferFailed();
    error BadThreshold();

    // ── 构造 ──

    /**
     * @param signers_   共识签名方名单。
     * @param threshold_ 放行所需的最少签名方数量。必须 >0 且 <= 名单长度。
     * @param minScore_  放行所需的最低评分（0-100）。
     */
    constructor(address[] memory signers_, uint256 threshold_, uint16 minScore_) {
        owner = msg.sender;
        _setSigners(signers_, threshold_, minScore_);
        cachedChainId = block.chainid;
        cachedDomainSeparator = _computeDomainSeparator();
    }

    modifier onlyOwner() {
        if (msg.sender != owner) revert NotOwner();
        _;
    }

    // ── 治理 ──

    function setSigners(address[] calldata signers_, uint256 threshold_, uint16 minScore_)
        external
        onlyOwner
    {
        _setSigners(signers_, threshold_, minScore_);
    }

    function setDepositsPaused(bool paused) external onlyOwner {
        depositsPaused = paused;
        emit DepositsPaused(paused);
    }

    function transferOwnership(address to) external onlyOwner {
        if (to == address(0)) revert ZeroAddress();
        emit OwnerChanged(owner, to);
        owner = to;
    }

    function _setSigners(address[] memory signers_, uint256 threshold_, uint16 minScore_) private {
        if (threshold_ == 0 || threshold_ > signers_.length) revert BadThreshold();
        if (minScore_ > 100) revert ScoreTooLow();

        for (uint256 i = 0; i < signerList.length; i++) {
            isSigner[signerList[i]] = false;
        }
        delete signerList;

        for (uint256 i = 0; i < signers_.length; i++) {
            address s = signers_[i];
            if (s == address(0)) revert ZeroAddress();
            // 名单里有重复地址会让阈值形同虚设——两个位置同一个人，
            // 一个私钥就能凑够两票。
            if (isSigner[s]) revert BadSignature();
            isSigner[s] = true;
            signerList.push(s);
        }
        threshold = threshold_;
        minScore = minScore_;
        emit SignersChanged(signers_, threshold_, minScore_);
    }

    function signers() external view returns (address[] memory) {
        return signerList;
    }

    // ── 入金 ──

    /**
     * @notice 为一笔工单托管资金。出币方先 approve 本合约。
     *
     * 用于条件支付的付款方，以及 OTC 里 taker 卖币的路径——两者都是
     * 「我的币要进托管」。
     */
    function deposit(
        bytes32 orderId,
        address token,
        uint256 amount,
        address beneficiary
    ) external {
        if (depositsPaused) revert DepositsArePaused();
        if (amount == 0) revert ZeroAmount();
        if (token == address(0) || beneficiary == address(0)) revert ZeroAddress();
        if (positions[orderId].status != Status.None) revert OrderExists();

        positions[orderId] = Position({
            token: token,
            amount: amount,
            payer: msg.sender,
            beneficiary: beneficiary,
            offerId: bytes32(0),
            status: Status.Escrowed
        });

        // 先记账后转账，且用实际到账量校验——手续费型代币（转账时扣税）
        // 会让 amount 与实际到账不符，那种代币不支持，这里直接拒绝而不是
        // 让仓位记着一个合约里并不存在的数。
        uint256 before = IERC20(token).balanceOf(address(this));
        _pull(token, msg.sender, amount);
        if (IERC20(token).balanceOf(address(this)) - before != amount) revert TransferFailed();

        emit Deposited(orderId, msg.sender, token, amount);
    }

    /**
     * @notice 做市方挂单时把币锁进来。挂出即锁币——买家看到的可成交量
     * 必须真的在合约里，不是一个数字。
     */
    function lockListing(bytes32 offerId, address token, uint256 amount) external {
        if (depositsPaused) revert DepositsArePaused();
        if (amount == 0) revert ZeroAmount();
        if (token == address(0)) revert ZeroAddress();

        ListingLock storage l = listings[offerId];
        if (l.maker == address(0)) {
            listings[offerId] = ListingLock({
                maker: msg.sender,
                token: token,
                total: amount,
                bound: 0,
                open: true
            });
        } else {
            // 同一个挂单加量：只有原 maker、同一种币、且挂单还开着才行
            if (l.maker != msg.sender || l.token != token || !l.open) revert ListingNotOpen();
            l.total += amount;
        }

        uint256 before = IERC20(token).balanceOf(address(this));
        _pull(token, msg.sender, amount);
        if (IERC20(token).balanceOf(address(this)) - before != amount) revert TransferFailed();

        emit ListingLocked(offerId, msg.sender, token, amount);
    }

    /**
     * @notice 下架，把还没被订单绑走的量退回给 maker。
     *
     * 已绑走的部分不能退——那些币属于具体的订单，得等那些订单走完。
     */
    function unlockListing(bytes32 offerId) external {
        ListingLock storage l = listings[offerId];
        if (l.maker != msg.sender || !l.open) revert ListingNotOpen();

        uint256 free = l.total - l.bound;
        l.open = false;
        l.total = l.bound;
        if (free > 0) _push(l.token, l.maker, free);
        emit ListingUnlocked(offerId, free);
    }

    /**
     * @notice 把挂单里锁好的一部分绑到一笔订单上。
     *
     * 买方向的 OTC 走这条路：币在挂单那一刻就进合约了，这里没有新的资金动作，
     * 只是把一块额度划给这笔订单。所以任何人都能调——它不转移资金，
     * 只是记账，而且必须由挂单的 maker 出币、收款方由参数指定。
     *
     * 谁能调不重要，因为它动不了钱：绑定只是把 maker 已锁的量划走一块，
     * 划走之后仍然只能通过带证明的 release/refund 才能真的转出去。
     */
    function bindListingLock(
        bytes32 orderId,
        bytes32 offerId,
        uint256 amount,
        address beneficiary
    ) external {
        if (amount == 0) revert ZeroAmount();
        if (beneficiary == address(0)) revert ZeroAddress();
        if (positions[orderId].status != Status.None) revert OrderExists();

        ListingLock storage l = listings[offerId];
        if (!l.open) revert ListingNotOpen();
        if (l.total - l.bound < amount) revert ListingInsufficient();

        l.bound += amount;
        positions[orderId] = Position({
            token: l.token,
            amount: amount,
            payer: l.maker,
            beneficiary: beneficiary,
            offerId: offerId,
            status: Status.Escrowed
        });

        emit ListingBound(offerId, orderId, amount);
    }

    // ── 异议 ──

    /**
     * @notice 提出异议。资金保持锁定，之后必须靠裁决证明才能动。
     * 只有这笔单的两方能提。
     */
    function raiseDispute(bytes32 orderId) external {
        Position storage p = positions[orderId];
        if (p.status == Status.None) revert NoPosition();
        if (p.status != Status.Escrowed) revert NotEscrowed();
        if (msg.sender != p.payer && msg.sender != p.beneficiary) revert NotParty();

        p.status = Status.Disputed;
        emit Disputed(orderId, msg.sender);
    }

    // ── 放行与退款：唯一的资金出口 ──

    /**
     * @notice 放行。要求共识证明：verdict = Release、score >= minScore、
     * 且有 threshold 个不同签名方签过名。
     *
     * @param att         共识证明。
     * @param signatures  按签名方地址**升序**排列的签名。升序是为了在
     *                    O(n) 内去重——同一个私钥签两次不该算两票。
     */
    function release(Attestation calldata att, bytes[] calldata signatures) external {
        Position storage p = positions[att.orderId];
        if (p.status == Status.None) revert NoPosition();
        if (p.status != Status.Escrowed) revert NotEscrowed();
        if (att.verdict != Verdict.Release) revert WrongVerdict();
        if (att.score < minScore) revert ScoreTooLow();

        _consume(att, signatures);

        p.status = Status.Released;
        _push(p.token, p.beneficiary, p.amount);
        emit Released(att.orderId, p.beneficiary, p.amount, att.score);
    }

    /**
     * @notice 退款。要求 verdict = Refund 的共识证明。
     *
     * 退款不看分数——条件没成立、超时、撤单都走这里，它们与「风险评分」无关。
     * 来自挂单锁仓的仓位退回挂单而不是个人余额：那批币是 maker 挂出去的，
     * 一笔订单没成不等于他要下架。
     */
    function refund(Attestation calldata att, bytes[] calldata signatures) external {
        Position storage p = positions[att.orderId];
        if (p.status == Status.None) revert NoPosition();
        if (p.status != Status.Escrowed) revert NotEscrowed();
        if (att.verdict != Verdict.Refund) revert WrongVerdict();

        _consume(att, signatures);
        _doRefund(att.orderId, p);
    }

    /**
     * @notice 裁决一笔有异议的单。要求 verdict 为 Release 或 Refund 的证明。
     *
     * Disputed 的单不能走 release/refund——那两个入口只接受 Escrowed，
     * 免得一份普通放行证明把有异议的单悄悄放掉。
     */
    function resolveDispute(Attestation calldata att, bytes[] calldata signatures) external {
        Position storage p = positions[att.orderId];
        if (p.status == Status.None) revert NoPosition();
        if (p.status != Status.Disputed) revert NotDisputed();

        _consume(att, signatures);

        if (att.verdict == Verdict.Release) {
            if (att.score < minScore) revert ScoreTooLow();
            p.status = Status.Released;
            _push(p.token, p.beneficiary, p.amount);
            emit Released(att.orderId, p.beneficiary, p.amount, att.score);
        } else if (att.verdict == Verdict.Refund) {
            _doRefund(att.orderId, p);
        } else {
            revert WrongVerdict();
        }
    }

    function _doRefund(bytes32 orderId, Position storage p) private {
        p.status = Status.Refunded;
        if (p.offerId != bytes32(0)) {
            ListingLock storage l = listings[p.offerId];
            if (l.open) {
                // 还回挂单：币留在合约里，只是重新变成可成交量
                l.bound -= p.amount;
                emit Refunded(orderId, address(this), p.amount);
                return;
            }
            // 挂单已下架：这部分当时算在 bound 里没退，现在还给 maker
            l.total -= p.amount;
            l.bound -= p.amount;
        }
        _push(p.token, p.payer, p.amount);
        emit Refunded(orderId, p.payer, p.amount);
    }

    // ── 证明校验 ──

    /// @dev 校验并作废一份证明。这是防重放的唯一执行点。
    function _consume(Attestation calldata att, bytes[] calldata signatures) private {
        if (block.timestamp > att.deadline) revert AttestationExpired();

        bytes32 digest = hashAttestation(att);
        if (attestationUsed[digest]) revert AttestationReplayed();
        // 先作废再转账。顺序是刻意的：即便下游有重入，这份证明也已经用掉了。
        attestationUsed[digest] = true;

        if (signatures.length < threshold) revert NotEnoughSigners();

        address last = address(0);
        uint256 counted = 0;
        for (uint256 i = 0; i < signatures.length; i++) {
            address rec = _recover(digest, signatures[i]);
            // 要求严格升序：既去重（同一个私钥签两次会得到同一个地址，
            // 不满足严格大于），又把去重做成 O(n)。
            if (rec <= last) revert SignersUnsorted();
            if (!isSigner[rec]) revert BadSignature();
            last = rec;
            counted++;
        }
        if (counted < threshold) revert NotEnoughSigners();
    }

    function hashAttestation(Attestation calldata att) public view returns (bytes32) {
        bytes32 structHash = keccak256(
            abi.encode(
                ATTESTATION_TYPEHASH,
                att.orderId,
                uint8(att.verdict),
                att.score,
                att.nonce,
                att.deadline
            )
        );
        return keccak256(abi.encodePacked("\x19\x01", domainSeparator(), structHash));
    }

    function domainSeparator() public view returns (bytes32) {
        // 分叉后 chainId 会变，缓存的域分隔符必须失效——否则一条链上的
        // 证明可以在另一条链上重放。
        if (block.chainid == cachedChainId) return cachedDomainSeparator;
        return _computeDomainSeparator();
    }

    function _computeDomainSeparator() private view returns (bytes32) {
        return keccak256(
            abi.encode(
                DOMAIN_TYPEHASH,
                keccak256("AtaraEscrow"),
                keccak256("1"),
                block.chainid,
                address(this)
            )
        );
    }

    /// @dev 从 65 字节签名恢复地址。拒绝高位 s 值，杜绝签名可塑性。
    function _recover(bytes32 digest, bytes calldata sig) private pure returns (address) {
        if (sig.length != 65) revert BadSignature();
        bytes32 r;
        bytes32 s;
        uint8 v;
        assembly {
            r := calldataload(sig.offset)
            s := calldataload(add(sig.offset, 32))
            v := byte(0, calldataload(add(sig.offset, 64)))
        }
        // 同一份签名翻转 s 与 v 仍然有效，会绕过按地址去重。只收下半区。
        if (uint256(s) > 0x7FFFFFFFFFFFFFFFFFFFFFFFFFFFFFFF5D576E7357A4501DDFE92F46681B20A0) {
            revert BadSignature();
        }
        if (v < 27) v += 27;
        if (v != 27 && v != 28) revert BadSignature();
        address rec = ecrecover(digest, v, r, s);
        if (rec == address(0)) revert BadSignature();
        return rec;
    }

    // ── 读 ──

    function positionOf(bytes32 orderId) external view returns (Position memory) {
        return positions[orderId];
    }

    function listingOf(bytes32 offerId) external view returns (ListingLock memory) {
        return listings[offerId];
    }

    /// @notice 挂单还剩多少可成交量。前端展示的就该是这个数。
    function listingAvailable(bytes32 offerId) external view returns (uint256) {
        ListingLock storage l = listings[offerId];
        if (!l.open) return 0;
        return l.total - l.bound;
    }

    // ── 转账 ──

    /// @dev 兼容返回 false 与不返回值两种 ERC-20 实现。
    function _pull(address token, address from, uint256 amount) private {
        (bool ok, bytes memory data) = token.call(
            abi.encodeWithSelector(IERC20.transferFrom.selector, from, address(this), amount)
        );
        if (!ok || (data.length != 0 && !abi.decode(data, (bool)))) revert TransferFailed();
    }

    function _push(address token, address to, uint256 amount) private {
        (bool ok, bytes memory data) =
            token.call(abi.encodeWithSelector(IERC20.transfer.selector, to, amount));
        if (!ok || (data.length != 0 && !abi.decode(data, (bool)))) revert TransferFailed();
    }
}
