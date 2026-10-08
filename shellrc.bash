# shellcheck shell=bash
# caboose's shell.d loader, for bash. ~/.bashrc sources it (layer-user.sh
# puts the line there), so every interactive bash -- caboose shell, a tmux
# window, the snapshot Claude Code's Bash tool takes of the shell -- reads
# ~/.config/caboose/shell.d, which the launcher mounts from the data dir's
# dot_config/caboose/shell.d. The snapshot then sets PATH to Claude Code's
# own, so a PATH a file sets reaches every shell but that one.
#
# What it reads, one order across both kinds, by name in byte order:
#   *.sh    for any shell caboose may offer (bash now, zsh later): plain
#           alias, export and function lines, valid in both
#   *.bash  for bash alone
# On a tie of name, X.sh comes before X.bash, so the bash-only file can build
# on the shared one: 10-a.sh 10-a.bash 20-b.sh 20-b.bash. Dotfiles and
# anything else (a README, an editor's backup) are skipped.
#
# At the top level, not in a function: a file sourced from a function has its
# `declare`s made local to that function, and they would vanish on return.
# So every name used here is __caboose_-prefixed and unset at the end.

if [ -d "$HOME/.config/caboose/shell.d" ]; then
    # The glob sorts by the collation; C is byte order, whatever the
    # locale. LC_ALL, since it would override LC_COLLATE, and put back.
    __caboose_lcall=${LC_ALL-__caboose_unset}
    LC_ALL=C
    __caboose_files=("$HOME"/.config/caboose/shell.d/*)
    if [ "$__caboose_lcall" = __caboose_unset ]; then
        unset LC_ALL
    else
        LC_ALL=$__caboose_lcall
    fi
    __caboose_done=/
    for __caboose_f in "${__caboose_files[@]}"; do
        __caboose_list=()
        case ${__caboose_f##*/} in
            *.sh)
                case $__caboose_done in *"/${__caboose_f##*/}/"*) continue ;; esac
                __caboose_list=("$__caboose_f")
                ;;
            *.bash)
                __caboose_sh=${__caboose_f%.bash}.sh
                if [ -f "$__caboose_sh" ]; then
                    __caboose_list=("$__caboose_sh")
                    __caboose_done="$__caboose_done${__caboose_sh##*/}/"
                fi
                __caboose_list+=("$__caboose_f")
                ;;
        esac
        for __caboose_g in "${__caboose_list[@]}"; do
            [ -f "$__caboose_g" ] && [ -r "$__caboose_g" ] || continue
            # No word on a non-zero status: `command -v x && alias ...`
            # ends that way wherever x is missing. The files are the
            # user's, so there is nothing for shellcheck to follow.
            # shellcheck source=/dev/null
            . "$__caboose_g"
        done
    done
    unset __caboose_lcall __caboose_files __caboose_done __caboose_f \
        __caboose_list __caboose_sh __caboose_g
fi
