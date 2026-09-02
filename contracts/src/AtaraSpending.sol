// SPDX-License-Identifier: MIT
pragma solidity 0.8.24;

interface IERC20Min {
    function transferFrom(address from, address to, uint256 amount) external returns (bool);
    function balanceOf(address account) external view returns (uint256);
}

/**
 * @title AtaraSpending
 * @notice 支配权策略：谁能花我的钱、单笔多少、窗口内多少、到什么时候。
 *
 * 为什么不能用 ERC-20 的 approve：那是一个扁平数字，没有周期窗口、
 * 没有单笔上限、没有到期。产品说的「额度」是一份可撤销、有周期、
 * 有单笔天花板的支配权——那需要一份策略合约来记和执行。
 *
 * 授权模型很简单，不需要阈值签名：**签发额度的人就是出钱的人**。
 * 账户自己调 grant，msg.sender 就是授权本身。这与托管合约不同——
 * 那里放行的判断权不在任何单一方手上，所以才需要共识证明。
 *
 * 花钱要两个条件同时成立：策略允许（这里执行），且账户对本合约
 * 做过 ERC-20 approve（代币合约执行）。缺一样都花不出去。
 * 这是刻意的双闸门：策略被绕过不了，approve 也随时能撤。
 */
contract AtaraSpending {
    struct Policy {
        /// @dev 出钱的账户。只有它能改和撤这份策略。
        address account;
        /// @dev 能花这笔钱的人或 agent。
        address spender;
        address token;
        /// @dev 单笔上限。
        uint256 perPayment;
        /// @dev 一个窗口内的总额。
        uint256 windowCap;
        /// @dev 窗口长度（秒）。weekly / monthly 由上层折算。
        uint64 cycleSecs;
        /// @dev 0 表示不过期。
        uint64 expiresAt;
        /// @dev 当前窗口已用。窗口滚动时归零。
        uint256 used;
        /// @dev 当前窗口的起点。
        uint64 windowStart;
        bool live;
    }

    mapping(bytes32 => Policy) private policies;

    event Granted(
        bytes32 indexed id,
        address indexed account,
        address indexed spender,
        address token,
        uint256 perPayment,
        uint256 windowCap,
        uint64 cycleSecs,
        uint64 expiresAt
    );
    event Revoked(bytes32 indexed id, address indexed by);
    event Spent(
        bytes32 indexed id, address indexed spender, address indexed to, uint256 amount, uint256 windowUsed
    );

    error NotAccount();
    error NotSpender();
    error NoPolicy();
    error NotLive();
    error Expired();
    error ZeroAmount();
    error ZeroAddress();
    error BadCaps();
    error BadCycle();
    error OverPerPayment();
    error OverWindow();
    error TransferFailed();

    // ── 签发与撤销 ──

    /**
     * @notice 签发或修改一份支配权。**只有出钱的账户能调**——
     * msg.sender 就是授权本身，不需要额外的签名证明。
     *
     * 修改时窗口用量保留：把上限调高不该顺带把这个窗口已经花掉的额度清零，
     * 否则「改一次额度」就成了绕过窗口的手段。
     */
    function grant(
        bytes32 id,
        address spender,
        address token,
        uint256 perPayment,
        uint256 windowCap,
        uint64 cycleSecs,
        uint64 expiresAt
    ) external {
        if (spender == address(0) || token == address(0)) revert ZeroAddress();
        if (perPayment == 0 || windowCap == 0) revert ZeroAmount();
        // 单笔上限超过窗口总额是配置错误：那条单笔限制永远不起作用。
        if (perPayment > windowCap) revert BadCaps();
        if (cycleSecs == 0) revert BadCycle();
        if (expiresAt != 0 && expiresAt <= block.timestamp) revert Expired();

        Policy storage p = policies[id];
        if (p.account == address(0)) {
            policies[id] = Policy({
                account: msg.sender,
                spender: spender,
                token: token,
                perPayment: perPayment,
                windowCap: windowCap,
                cycleSecs: cycleSecs,
                expiresAt: expiresAt,
                used: 0,
                windowStart: uint64(block.timestamp),
                live: true
            });
        } else {
            if (p.account != msg.sender) revert NotAccount();
            // 换币种等于换一份策略，用量不该跟着过来
            if (p.token != token) {
                p.used = 0;
                p.windowStart = uint64(block.timestamp);
            }
            p.spender = spender;
            p.token = token;
            p.perPayment = perPayment;
            p.windowCap = windowCap;
            p.cycleSecs = cycleSecs;
            p.expiresAt = expiresAt;
            p.live = true;
        }

        emit Granted(id, msg.sender, spender, token, perPayment, windowCap, cycleSecs, expiresAt);
    }

    /**
     * @notice 撤销。只有账户本人能撤。
     *
     * 撤销不清零 used——万一之后又签发同一个 id，那个窗口里已经花掉的
     * 不该凭空回来。
     */
    function revoke(bytes32 id) external {
        Policy storage p = policies[id];
        if (p.account == address(0)) revert NoPolicy();
        if (p.account != msg.sender) revert NotAccount();
        p.live = false;
        emit Revoked(id, msg.sender);
    }

    // ── 花钱 ──

    /**
     * @notice 按策略花钱。只有被授权的 spender 能调。
     *
     * 三道闸门：策略有效且未过期、单笔不超上限、窗口余量够。
     * 第四道在代币合约上——账户必须对本合约 approve 过，否则 transferFrom
     * 失败。策略和 approve 任何一个撤掉，钱就花不出去。
     */
    function spend(bytes32 id, uint256 amount, address to) external {
        if (amount == 0) revert ZeroAmount();
        if (to == address(0)) revert ZeroAddress();

        Policy storage p = policies[id];
        if (p.account == address(0)) revert NoPolicy();
        if (!p.live) revert NotLive();
        if (msg.sender != p.spender) revert NotSpender();
        if (p.expiresAt != 0 && block.timestamp > p.expiresAt) revert Expired();
        if (amount > p.perPayment) revert OverPerPayment();

        // 窗口滚动：跨过整数个周期就重开一个窗口。
        // 按整数倍前推而不是设成 now，窗口边界才是稳定的——否则每次花钱
        // 都把窗口往后推，等效于窗口永远不结束。
        uint64 nowTs = uint64(block.timestamp);
        if (nowTs >= p.windowStart + p.cycleSecs) {
            uint64 elapsed = nowTs - p.windowStart;
            p.windowStart += (elapsed / p.cycleSecs) * p.cycleSecs;
            p.used = 0;
        }
        if (p.used + amount > p.windowCap) revert OverWindow();

        p.used += amount;

        // 先记账后转账，且校验实际到账——手续费型代币会让账实不符
        uint256 before = IERC20Min(p.token).balanceOf(to);
        (bool ok, bytes memory data) = p.token.call(
            abi.encodeWithSelector(IERC20Min.transferFrom.selector, p.account, to, amount)
        );
        if (!ok || (data.length != 0 && !abi.decode(data, (bool)))) revert TransferFailed();
        if (IERC20Min(p.token).balanceOf(to) - before != amount) revert TransferFailed();

        emit Spent(id, msg.sender, to, amount, p.used);
    }

    // ── 读 ──

    function policyOf(bytes32 id) external view returns (Policy memory) {
        return policies[id];
    }

    /**
     * @notice 当前窗口还剩多少能花。
     *
     * 会把窗口滚动算进去——不然刚跨过周期时会读到 0，而实际是满额。
     * 已撤销、已过期、不存在一律返回 0。
     */
    function available(bytes32 id) external view returns (uint256) {
        Policy storage p = policies[id];
        if (p.account == address(0) || !p.live) return 0;
        if (p.expiresAt != 0 && block.timestamp > p.expiresAt) return 0;

        uint256 used = p.used;
        if (uint64(block.timestamp) >= p.windowStart + p.cycleSecs) {
            used = 0;
        }
        if (used >= p.windowCap) return 0;
        uint256 left = p.windowCap - used;
        return left < p.perPayment ? left : p.perPayment;
    }

    /// @notice 这份策略此刻还能不能花钱。撤销、过期、不存在都是不能。
    function isLive(bytes32 id) external view returns (bool) {
        Policy storage p = policies[id];
        if (p.account == address(0) || !p.live) return false;
        return p.expiresAt == 0 || block.timestamp <= p.expiresAt;
    }
}
